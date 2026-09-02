package ota

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	ipc "github.com/librescoot/redis-ipc"

	"github.com/librescoot/bluetooth-service/pkg/logger"
)

// FrameWriter sends a raw USOCK frame towards the nRF (and thus the phone).
type FrameWriter interface {
	WriteWithFrameID(frameID byte, data []byte) error
}

// Installer queues a completed, verified bundle for installation.
type Installer interface {
	Install(component byte, path string) error
}

// frameOTAStatus mirrors ble.FrameOTAStatus without importing pkg/ble.
const frameOTAStatus = 0xB2

// Receiver tuning; announced to the phone in START_ACK so the constants live
// in exactly one place.
const (
	windowChunks = 64                     // phone keeps at most this many chunks in flight (~15 KiB)
	ackEvery     = 16                     // cumulative ACK every N in-order chunks...
	ackInterval  = 250 * time.Millisecond // ...or at least this often while data flows
	syncEvery    = 256 << 10              // fdatasync the staging file every 256 KiB
	idleTimeout  = 10 * time.Minute       // release the power inhibitor after this much silence
	rewindMinGap = 100 * time.Millisecond // rate limit for REWIND ACKs on an offset gap

	inhibitorID      = "ble-ota"
	inhibitorTimeout = 30 * time.Minute // safety release if install feedback never arrives
	statusHash       = "ota:ble"
	installHash      = "ota" // written by update-service's status reporter
	progressMinGap   = 100 * time.Millisecond
)

type recvState int

const (
	stateIdle recvState = iota
	stateReceiving
	stateInstalling
)

type session struct {
	start          Start
	file           *os.File
	expected       uint32 // next expected offset == bytes written so far
	bytesSinceSync uint32
	chunksSinceAck int
	lastAck        time.Time
	lastRewind     time.Time

	// rolling transfer rate, recomputed roughly once per second
	rateMark time.Time
	rateBase uint32
	rateBps  uint32
}

// Receiver implements the scooter-side OTA transfer state machine. HandleData
// and HandleControl are invoked synchronously from the USOCK read loop; all
// other entry points (Redis watcher, timers) synchronize on mu.
type Receiver struct {
	log    *logger.Logger
	ipcCli *ipc.Client // may be nil in unit tests
	writer func() FrameWriter
	inst   map[byte]Installer

	// installedVersion returns version:<comp>[version_id] for a component,
	// "" when unknown; replaced in tests
	installedVersion func(component byte) string

	staging *Staging

	mu              sync.Mutex
	state           recvState
	sess            *session
	installComp     byte
	installBundleID string
	lastPhase       byte
	lastPercent     byte
	watcher         *ipc.HashWatcher
	waitingOutcome  bool // install finished, still observing status until it clears to idle
	idleTimer       *time.Timer
	inhibitTmr      *time.Timer
	lastSent        time.Time // pacing for INSTALL_PROGRESS notifications
}

// New creates a Receiver. writer returns the current frame writer (the USOCK
// connection is replaced on baud changes, hence the indirection); installers
// maps component ids to install backends.
func New(log *logger.Logger, ipcCli *ipc.Client, writer func() FrameWriter, stagingDir string, installers map[byte]Installer) *Receiver {
	if stagingDir == "" {
		stagingDir = DefaultStagingDir
	}
	r := &Receiver{
		log:       log,
		ipcCli:    ipcCli,
		writer:    writer,
		inst:      installers,
		staging:   NewStaging(stagingDir),
		lastPhase: PhaseIdle,
	}
	r.installedVersion = func(component byte) string {
		if ipcCli == nil {
			return ""
		}
		v, err := ipcCli.HGet("version:"+componentName(component), "version_id")
		if err != nil {
			return ""
		}
		return v
	}
	return r
}

// send transmits a status message to the phone; drops silently when the link
// is down (cumulative ACKs are loss-tolerant by design).
func (r *Receiver) send(payload []byte) {
	w := r.writer()
	if w == nil {
		return
	}
	if err := w.WriteWithFrameID(frameOTAStatus, payload); err != nil {
		r.log.Debugf("OTA: status send failed: %v", err)
	}
}

// HandleData processes a DATA frame: [offset:u32][chunk].
func (r *Receiver) HandleData(payload []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.state != stateReceiving || r.sess == nil {
		return
	}
	s := r.sess

	offset, data, err := DecodeData(payload)
	if err != nil {
		r.log.Warnf("OTA: bad DATA frame: %v", err)
		return
	}

	if offset != s.expected {
		// gap or duplicate: go-back-N — tell the phone where to resume from
		if time.Since(s.lastRewind) >= rewindMinGap {
			s.lastRewind = time.Now()
			r.log.Debugf("OTA: offset gap (got %d, expected %d), requesting rewind", offset, s.expected)
			r.send(EncodeAck(AckFlagRewind, s.expected))
		}
		return
	}

	remaining := s.start.TotalSize - s.expected
	if uint32(len(data)) > remaining {
		r.log.Warnf("OTA: chunk overruns declared total (len=%d, remaining=%d)", len(data), remaining)
		r.send(EncodeError(ErrInternal, "chunk overruns total"))
		return
	}

	if _, err := s.file.Write(data); err != nil {
		r.log.Errorf("OTA: staging write failed: %v", err)
		r.send(EncodeError(ErrWriteFailed, "staging write failed"))
		r.abortSessionLocked(true)
		return
	}
	s.expected += uint32(len(data))
	s.bytesSinceSync += uint32(len(data))
	s.chunksSinceAck++

	if s.bytesSinceSync >= syncEvery {
		s.bytesSinceSync = 0
		if err := s.file.Sync(); err != nil {
			r.log.Warnf("OTA: fdatasync failed: %v", err)
		}
	}

	r.touchIdleLocked()

	// rolling transfer rate for the ota:ble status hash
	if s.rateMark.IsZero() {
		s.rateMark = time.Now()
		s.rateBase = s.expected
	} else if elapsed := time.Since(s.rateMark); elapsed >= time.Second {
		s.rateBps = uint32(float64(s.expected-s.rateBase) / elapsed.Seconds())
		s.rateMark = time.Now()
		s.rateBase = s.expected
	}

	final := s.expected == s.start.TotalSize
	if final || s.chunksSinceAck >= ackEvery || time.Since(s.lastAck) >= ackInterval {
		s.chunksSinceAck = 0
		s.lastAck = time.Now()
		r.send(EncodeAck(0, s.expected))
		r.publishStatusLocked("receiving")
	}
}

// HandleControl processes a control frame (START/COMPLETE/ABORT/STATUS_REQ).
func (r *Receiver) HandleControl(payload []byte) {
	if len(payload) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	switch payload[0] {
	case OpStart:
		r.handleStartLocked(payload)
	case OpComplete:
		r.handleCompleteLocked()
	case OpAbort:
		r.handleAbortLocked()
	case OpStatusReq:
		// With no session and no pending outcome, answer (and re-latch) idle:
		// the OTA_STATUS characteristic stores the last notified value on the nRF
		// across connections, so without this an idle receiver would keep serving a
		// stale terminal phase (failure, stalled "installing") to a reconnecting
		// phone and every polling client, even though a new transfer is possible.
		if r.state == stateIdle && !r.waitingOutcome {
			r.lastPhase, r.lastPercent = PhaseIdle, 0
			r.send(EncodeInstallProgress(PhaseIdle, 0, ""))
		} else {
			r.send(EncodeInstallProgress(r.lastPhase, r.lastPercent, ""))
		}
	default:
		r.log.Warnf("OTA: unknown control opcode 0x%02x", payload[0])
	}
}

func (r *Receiver) handleStartLocked(payload []byte) {
	start, err := DecodeStart(payload)
	if err != nil {
		r.log.Warnf("OTA: bad START: %v", err)
		r.send(EncodeStartAck(StartBadParams, 0, windowChunks, ackEvery, MaxChunkSize))
		return
	}

	if r.state == stateInstalling {
		r.send(EncodeStartAck(StartInstalling, 0, windowChunks, ackEvery, MaxChunkSize))
		return
	}

	if start.Version != 1 ||
		start.ChunkSize == 0 || start.ChunkSize > MaxChunkSize ||
		start.TotalSize == 0 ||
		r.inst[start.Component] == nil {
		r.log.Warnf("OTA: unsupported START (ver=%d comp=%d chunk=%d total=%d)",
			start.Version, start.Component, start.ChunkSize, start.TotalSize)
		r.send(EncodeStartAck(StartBadParams, 0, windowChunks, ackEvery, MaxChunkSize))
		return
	}

	// Refuse a bundle for the version that board already runs here, at the
	// cost of one round trip, instead of after the whole transfer when
	// update-service would refuse it anyway.
	if target := versionFromBundleID(start.BundleID); target != "" {
		if installed := r.installedVersion(start.Component); sameVersion(target, installed) {
			r.log.Warnf("OTA: %s is already installed on %s, refusing START", target, componentName(start.Component))
			r.send(EncodeStartAck(StartAlreadyInstalled, 0, windowChunks, ackEvery, MaxChunkSize))
			return
		}
	}

	// a START while receiving replaces the session: either the phone
	// reconnected (same bundle -> resume) or switched to a newer bundle
	if r.state == stateReceiving && r.sess != nil {
		r.log.Infof("OTA: new START while receiving %s; closing old session", r.sess.start.BundleID)
		r.closeSessionFileLocked()
	}

	// An accepted START ends observation of a previous install's outcome
	// (e.g. a DBC update awaiting its next power cycle): the phone has moved
	// on, and a stale status transition must not clobber the new transfer's
	// progress relay.
	if r.watcher != nil {
		_ = r.watcher.Stop()
		r.watcher = nil
		r.waitingOutcome = false
	}

	// A fresh transfer must not inherit a stale terminal phase from a previous
	// session (failure / stuck installing). Clear it so the progress relay for this
	// transfer starts clean.
	r.lastPhase = PhaseIdle
	r.lastPercent = 0

	shaHex := hex.EncodeToString(start.SHA256[:])
	resume := uint32(0)
	if sc := r.staging.LoadSidecar(start.Component, start.BundleID); sc != nil &&
		sc.SHA256 == shaHex &&
		sc.TotalSize == start.TotalSize &&
		sc.ChunkSize == start.ChunkSize {
		size := r.staging.PartSize(start.Component, start.BundleID)
		// round down to a chunk boundary: an unsynced tail may be torn after a
		// hard reboot, and partial chunks complicate the sender for no gain
		resume = size - size%uint32(start.ChunkSize)
		if resume > start.TotalSize {
			resume = 0
		}
	} else {
		r.staging.Discard(start.Component, start.BundleID)
	}

	if resume == 0 {
		need := uint64(start.TotalSize)
		if !r.staging.HasSpace(need) {
			r.staging.CleanupStale()
			if !r.staging.HasSpace(need) {
				r.log.Warnf("OTA: not enough staging space for %d bytes", start.TotalSize)
				r.send(EncodeStartAck(StartNoSpace, 0, windowChunks, ackEvery, MaxChunkSize))
				return
			}
		}
		if err := r.staging.WriteSidecar(start.Component, &Sidecar{
			BundleID:  start.BundleID,
			Component: componentName(start.Component),
			TotalSize: start.TotalSize,
			SHA256:    shaHex,
			ChunkSize: start.ChunkSize,
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			r.log.Errorf("OTA: sidecar write failed: %v", err)
			r.send(EncodeStartAck(StartBadParams, 0, windowChunks, ackEvery, MaxChunkSize))
			return
		}
	}

	path := r.staging.PartPath(start.Component, start.BundleID)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		r.log.Errorf("OTA: opening staging file failed: %v", err)
		r.send(EncodeStartAck(StartBadParams, 0, windowChunks, ackEvery, MaxChunkSize))
		return
	}
	if err := f.Truncate(int64(resume)); err != nil {
		_ = f.Close()
		r.log.Errorf("OTA: truncating staging file failed: %v", err)
		r.send(EncodeStartAck(StartBadParams, 0, windowChunks, ackEvery, MaxChunkSize))
		return
	}
	if _, err := f.Seek(int64(resume), io.SeekStart); err != nil {
		_ = f.Close()
		r.log.Errorf("OTA: seeking staging file failed: %v", err)
		r.send(EncodeStartAck(StartBadParams, 0, windowChunks, ackEvery, MaxChunkSize))
		return
	}

	r.sess = &session{
		start:    start,
		file:     f,
		expected: resume,
		lastAck:  time.Now(),
	}
	r.state = stateReceiving
	r.setInhibitorLocked(fmt.Sprintf("BLE OTA transfer of %s", start.BundleID))
	r.touchIdleLocked()
	r.publishStatusLocked("receiving")

	status := byte(StartNew)
	if resume > 0 {
		status = StartResume
	}
	r.log.Infof("OTA: transfer of %s (%s, %d bytes) starting at offset %d",
		start.BundleID, componentName(start.Component), start.TotalSize, resume)
	r.send(EncodeStartAck(status, resume, windowChunks, ackEvery, MaxChunkSize))
}

func (r *Receiver) handleCompleteLocked() {
	if r.state != stateReceiving || r.sess == nil {
		r.send(EncodeError(ErrNoSession, "no active transfer"))
		return
	}
	s := r.sess

	if s.expected != s.start.TotalSize {
		r.log.Warnf("OTA: COMPLETE at %d of %d bytes", s.expected, s.start.TotalSize)
		r.send(EncodeCompleteAck(CompleteSizeMismatch))
		return
	}

	if err := s.file.Sync(); err != nil {
		r.log.Errorf("OTA: final fsync failed: %v", err)
	}

	r.send(EncodeInstallProgress(PhaseVerifying, 0, "verifying sha256"))
	r.lastPhase = PhaseVerifying
	sum, err := fileSHA256(s.file)
	if err != nil {
		r.log.Errorf("OTA: hashing staged bundle failed: %v", err)
		r.send(EncodeCompleteAck(CompleteSHAMismatch))
		r.abortSessionLocked(true)
		return
	}
	if sum != hex.EncodeToString(s.start.SHA256[:]) {
		r.log.Warnf("OTA: SHA-256 mismatch (got %s)", sum)
		r.send(EncodeCompleteAck(CompleteSHAMismatch))
		r.abortSessionLocked(true)
		return
	}

	start := s.start
	r.closeSessionFileLocked()

	partPath := r.staging.PartPath(start.Component, start.BundleID)
	finalPath := r.staging.FinalPath(start.Component, start.BundleID)
	if err := os.Rename(partPath, finalPath); err != nil {
		r.log.Errorf("OTA: renaming staged bundle failed: %v", err)
		r.send(EncodeCompleteAck(CompleteQueueFailed))
		r.releaseSessionLocked()
		return
	}

	if err := r.inst[start.Component].Install(start.Component, finalPath); err != nil {
		r.log.Errorf("OTA: queueing install failed: %v", err)
		r.send(EncodeCompleteAck(CompleteQueueFailed))
		r.releaseSessionLocked()
		return
	}

	r.log.Infof("OTA: %s verified and queued for install", start.BundleID)
	r.state = stateInstalling
	r.installComp = start.Component
	r.installBundleID = start.BundleID
	r.sess = nil
	r.lastPhase = PhaseInstalling
	r.lastPercent = 0
	r.publishStatusLocked("installing")
	r.startInstallWatcherLocked()
	r.armInhibitorTimeoutLocked()
	r.send(EncodeCompleteAck(CompleteOK))
}

func (r *Receiver) handleAbortLocked() {
	if r.state == stateReceiving {
		r.log.Infof("OTA: transfer aborted by phone")
		// keep the partial file and sidecar: the user may resume later
		r.abortSessionLocked(false)
	}
	r.send(EncodeAbortAck())
}

// abortSessionLocked tears down a receiving session; discard removes staging.
func (r *Receiver) abortSessionLocked(discard bool) {
	if r.sess != nil && discard {
		r.staging.Discard(r.sess.start.Component, r.sess.start.BundleID)
	}
	r.closeSessionFileLocked()
	r.releaseSessionLocked()
}

func (r *Receiver) closeSessionFileLocked() {
	if r.sess != nil && r.sess.file != nil {
		if err := r.sess.file.Close(); err != nil {
			r.log.Warnf("OTA: closing staging file failed: %v", err)
		}
		r.sess.file = nil
	}
}

// releaseSessionLocked returns to idle: drops the session, the inhibitor and
// the idle timer.
func (r *Receiver) releaseSessionLocked() {
	r.closeSessionFileLocked()
	r.sess = nil
	r.state = stateIdle
	r.lastPhase = PhaseIdle
	r.lastPercent = 0
	if r.idleTimer != nil {
		r.idleTimer.Stop()
		r.idleTimer = nil
	}
	r.removeInhibitorLocked()
	r.publishStatusLocked("idle")
	// Overwrite the OTA_STATUS characteristic: the nRF stores the last notified
	// value across connections, so an idle session must push an idle frame to
	// clear whatever terminal frame (e.g. a failure) the previous session left.
	r.send(EncodeInstallProgress(PhaseIdle, 0, ""))
}

func (r *Receiver) touchIdleLocked() {
	if r.idleTimer != nil {
		r.idleTimer.Stop()
	}
	r.idleTimer = time.AfterFunc(idleTimeout, r.onIdleTimeout)
}

func (r *Receiver) onIdleTimeout() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != stateReceiving {
		return
	}
	r.log.Infof("OTA: transfer idle for %s; releasing resources (staging kept for resume)", idleTimeout)
	r.abortSessionLocked(false)
}

// fileSHA256 hashes the file from the beginning; the offset is left undefined.
func fileSHA256(f *os.File) (string, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

////////////////////////////////////////////////////////////////////////////////
// Install progress relay (update-service "ota" hash -> phone notifications)
////////////////////////////////////////////////////////////////////////////////

func (r *Receiver) startInstallWatcherLocked() {
	if r.ipcCli == nil {
		return
	}
	comp := componentName(r.installComp)
	if r.watcher != nil {
		// A fresh install begins while we may still be observing the outcome
		// of a previous one (e.g. a DBC update awaiting its next power cycle).
		// Replace the watcher so it tracks this component instead.
		_ = r.watcher.Stop()
		r.watcher = nil
		r.waitingOutcome = false
	}
	w := r.ipcCli.NewHashWatcher(installHash)
	w.OnAny(func(field, value string) error {
		r.onInstallField(comp, field, value)
		return nil
	})
	if err := w.Start(); err != nil {
		r.log.Errorf("OTA: starting install watcher failed: %v", err)
		return
	}
	r.watcher = w
}

func (r *Receiver) stopInstallWatcherLocked() {
	if r.watcher != nil {
		// The watcher is a progress relay; once we're tearing it down there
		// is no listener left to hand a stop failure to.
		_ = r.watcher.Stop()
		r.watcher = nil
	}
	if r.inhibitTmr != nil {
		r.inhibitTmr.Stop()
		r.inhibitTmr = nil
	}
}

func (r *Receiver) onInstallField(comp, field, value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Relay progress while actively installing, and keep relaying a successful
	// outcome (pending-reboot/success) until the component's status returns to
	// idle. For MDB the reboot restarts this receiver and resets the phase; the
	// DBC applies on its next power cycle without rebooting the MDB, so without
	// this the reported phase would stay latched at pending-reboot forever.
	if r.state != stateInstalling && !r.waitingOutcome {
		return
	}

	switch field {
	case "install-progress:" + comp:
		if r.waitingOutcome {
			return
		}
		if pct, err := strconv.Atoi(value); err == nil && pct >= 0 && pct <= 100 {
			r.lastPhase = PhaseInstalling
			r.lastPercent = byte(pct)
			r.sendProgressPacedLocked("")
		}

	case "status:" + comp:
		switch value {
		case "preparing", "installing":
			if r.waitingOutcome {
				return
			}
			if value == "preparing" {
				r.lastPhase = PhaseVerifying
			} else {
				r.lastPhase = PhaseInstalling
			}
			r.sendProgressPacedLocked(value)
		case "pending-reboot":
			r.lastPhase = PhasePendingReboot
			r.lastPercent = 100
			// update-service reboots the MDB after 3 min of sustained stand-by
			// (updater.go TriggerReboot); the DBC applies on its next power-on.
			// Either way update-service owns the reboot coordination from here.
			msg := "reboots after 3 min in stand-by"
			if comp == "dbc" {
				msg = "applies on next power-on"
			}
			r.send(EncodeInstallProgress(r.lastPhase, r.lastPercent, msg))
			// install is done: free the staged bundle and our inhibitor;
			// update-service owns the reboot coordination from here
			r.finishInstallLocked()
		case "complete", "installed", "success":
			r.lastPhase = PhaseSuccess
			r.lastPercent = 100
			r.send(EncodeInstallProgress(r.lastPhase, r.lastPercent, ""))
			r.finishInstallLocked()
		case "idle":
			if !r.waitingOutcome {
				// We were tracking an install (stateInstalling) but update-service
				// cleared to idle without a terminal outcome — e.g. it had nothing
				// to install, or the install was superseded. Leaving the receiver in
				// stateInstalling here would block every new transfer (START answers
				// StartInstalling) and keep OTA_STATUS stuck on the install phase.
				// Finish as a non-installed outcome, which returns to idle.
				if r.state == stateInstalling {
					r.finishInstallLocked()
				}
				return
			}
			// The target applied the update (DBC next power-on, or MDB reboot
			// completed) and cleared its status: the pending outcome is over.
			r.lastPhase = PhaseIdle
			r.lastPercent = 0
			r.waitingOutcome = false
			r.send(EncodeInstallProgress(r.lastPhase, r.lastPercent, ""))
			r.stopInstallWatcherLocked()
		case "error":
			r.lastPhase = PhaseFailure
			msg, _ := r.watcherFetch("error-message:" + comp)
			r.send(EncodeInstallProgress(PhaseFailure, 0, msg))
			r.finishInstallLocked()
		}
	}
}

// watcherFetch reads a field from the install hash (best effort).
func (r *Receiver) watcherFetch(field string) (string, bool) {
	if r.watcher == nil {
		return "", false
	}
	v, err := r.watcher.Fetch(field)
	if err != nil {
		return "", false
	}
	return v, true
}

// finishInstallLocked ends the install phase. The lastPhase/lastPercent values
// are kept so a reconnecting phone gets the outcome via STATUS_REQ.
func (r *Receiver) finishInstallLocked() {
	phase, pct := r.lastPhase, r.lastPercent

	// A successfully installed MDB full image stays in place: it now lives in
	// update-service's download dir and is the base for future delta updates
	// (update-service's retention keeps the newest). Everything else is
	// discarded: failures need a fresh transfer, DBC bundles have been copied
	// to the DBC by the handoff, and .delta files are consumed by
	// update-service during apply.
	installed := phase == PhasePendingReboot || phase == PhaseSuccess
	if installed && r.installComp == ComponentMDB && !strings.HasSuffix(r.installBundleID, ".delta") {
		r.staging.Forget(r.installComp, r.installBundleID)
	} else {
		r.staging.Discard(r.installComp, r.installBundleID)
	}
	r.installBundleID = ""

	// The inhibitor is released below; its watchdog must not fire later.
	if r.inhibitTmr != nil {
		r.inhibitTmr.Stop()
		r.inhibitTmr = nil
	}

	r.state = stateIdle
	r.sess = nil
	r.removeInhibitorLocked()
	r.publishStatusLocked("idle")

	// A successful install leaves the target in pending-reboot: the MDB is
	// rebooted by update-service (which restarts this receiver and resets the
	// phase), but the DBC only applies on its next power cycle. Keep observing
	// the component's status so the phase reported to the phone is cleared back
	// to idle when that happens, keeping this outcome observable via STATUS_REQ.
	if installed {
		r.lastPhase, r.lastPercent = phase, pct
		r.waitingOutcome = true
	} else {
		// Errors and stalled installs end observation. Their terminal phase must not
		// latch on the OTA_STATUS characteristic (the nRF keeps the last notified
		// value across connections) or it would block/confuse every later client.
		// Clear it and push an idle frame, the same signal STATUS_REQ answers.
		r.lastPhase, r.lastPercent = PhaseIdle, 0
		r.waitingOutcome = false
		r.stopInstallWatcherLocked()
		r.send(EncodeInstallProgress(PhaseIdle, 0, ""))
	}
}

func (r *Receiver) sendProgressPacedLocked(msg string) {
	// the SoftDevice notification queue is shallow; pace progress updates
	if time.Since(r.lastSent) < progressMinGap {
		return
	}
	r.lastSent = time.Now()
	r.send(EncodeInstallProgress(r.lastPhase, r.lastPercent, msg))
}

////////////////////////////////////////////////////////////////////////////////
// Power inhibitor & status hash
////////////////////////////////////////////////////////////////////////////////

// powerInhibit mirrors the JSON shape pm-service expects in power:inhibits
// (same as bluetooth-service's DFU inhibitor and update-service's inhibitors).
type powerInhibit struct {
	ID      string `json:"id"`
	Who     string `json:"who"`
	What    string `json:"what"`
	Why     string `json:"why"`
	Type    string `json:"type"`
	Created int64  `json:"created"`
}

func (r *Receiver) setInhibitorLocked(why string) {
	if r.ipcCli == nil {
		return
	}
	data, err := json.Marshal(powerInhibit{
		ID:      inhibitorID,
		Who:     "bluetooth-service",
		What:    "ble-ota-transfer",
		Why:     why,
		Type:    "block",
		Created: time.Now().Unix(),
	})
	if err != nil {
		return
	}
	if err := r.ipcCli.Hash("power:inhibits").Set(inhibitorID, string(data)); err != nil {
		r.log.Warnf("OTA: setting power inhibitor failed: %v", err)
	}
}

func (r *Receiver) removeInhibitorLocked() {
	if r.ipcCli == nil {
		return
	}
	if err := r.ipcCli.Hash("power:inhibits").Delete(inhibitorID); err != nil {
		r.log.Warnf("OTA: removing power inhibitor failed: %v", err)
	}
}

// armInhibitorTimeoutLocked guards against update-service never reporting a
// terminal state: the inhibitor must not outlive the install indefinitely.
func (r *Receiver) armInhibitorTimeoutLocked() {
	if r.inhibitTmr != nil {
		r.inhibitTmr.Stop()
	}
	r.inhibitTmr = time.AfterFunc(inhibitorTimeout, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.state == stateInstalling {
			r.log.Warnf("OTA: no install feedback after %s; releasing inhibitor", inhibitorTimeout)
			r.finishInstallLocked()
		}
	})
}

func (r *Receiver) publishStatusLocked(state string) {
	if r.ipcCli == nil {
		return
	}
	fields := map[string]any{
		"state":      state,
		"updated-at": strconv.FormatInt(time.Now().Unix(), 10),
	}
	if r.sess != nil {
		fields["bundle-id"] = r.sess.start.BundleID
		fields["component"] = componentName(r.sess.start.Component)
		fields["received-bytes"] = strconv.FormatUint(uint64(r.sess.expected), 10)
		fields["total-bytes"] = strconv.FormatUint(uint64(r.sess.start.TotalSize), 10)
		fields["rate-bps"] = strconv.FormatUint(uint64(r.sess.rateBps), 10)
	}
	if err := r.ipcCli.Hash(statusHash).SetMany(fields); err != nil {
		r.log.Debugf("OTA: publishing status failed: %v", err)
	}
}
