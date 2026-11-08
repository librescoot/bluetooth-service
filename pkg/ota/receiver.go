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

// Stager copies a completed, verified bundle into a component's own update
// directory without installing it, so a later StagedApplier can install whatever
// is staged there. It is asynchronous for components whose update directory
// lives on another board, and reports progress through the ota hash.
type Stager interface {
	Stage(component byte, path string) error
}

// StagedApplier installs everything staged in a component's update directory,
// the equivalent of update-service's `apply-staged-updates` command.
// Asynchronous like Stager.
type StagedApplier interface {
	ApplyStaged(component byte) error
}

// StagedPruner drops a component's local copies of artifacts that have been
// installed elsewhere. Only cross-board staging needs it: the copies are
// transport, and the board that received them keeps its own.
type StagedPruner interface {
	PruneStaged(component byte)
}

// frameOTAStatus mirrors ble.FrameOTAStatus without importing pkg/ble.
const frameOTAStatus = 0xB2

// handoffFieldPrefix names the liveness field a cross-board handoff writes
// while it runs (waiting for the dashboard to boot, copying an artifact to
// it). The receiver re-arms its install watchdog on it and relays it as
// progress, so a handoff that takes minutes is not mistaken for a dead
// update-service. It is not an update-service field and does not touch the
// status or error fields the commit gate reads.
const handoffFieldPrefix = "handoff:"

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

// installFeedbackTimeout is how long update-service may stay silent about a
// queued install before the phone is told it never started. update-service
// heartbeats every 30 s for the whole operation, so silence this long means the
// request was dropped (busy updater, open commit gate, dead service) rather
// than an install making slow progress. Without it the receiver would sit in
// "installing" for the full inhibitorTimeout and refuse every START in the
// meantime, which is what makes a scooter restart the only way out. A variable
// so tests can shorten it.
var installFeedbackTimeout = 2 * time.Minute

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
	feedbackTmr     *time.Timer
	installQueuedAt int64     // unix seconds the install was queued, for refusal freshness
	installStaged   bool      // the running install came from APPLY_STAGED
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

func (r *Receiver) Busy() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state != stateIdle
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
	case OpStart, OpStartStaged:
		r.handleStartLocked(payload)
	case OpComplete:
		r.handleCompleteLocked()
	case OpAbort:
		r.handleAbortLocked()
	case OpApplyStaged:
		component, err := DecodeApplyStaged(payload)
		if err != nil {
			r.log.Warnf("OTA: %v", err)
			r.send(EncodeError(ErrInternal, "bad APPLY_STAGED"))
			return
		}
		if r.inst[component] == nil {
			r.send(EncodeError(ErrInternal, "unsupported component"))
			return
		}
		if r.state == stateInstalling {
			r.send(EncodeError(ErrInternal, "an install is already running"))
			return
		}
		// A staged install replaces any interrupted transfer: that data is
		// untouched in the component directory and the phone can add to it
		// later, but the session is closed so no partial write races the apply.
		if r.state == stateReceiving {
			r.abortSessionLocked(false)
		}
		if r.watcher != nil {
			_ = r.watcher.Stop()
			r.watcher = nil
			r.waitingOutcome = false
		}
		r.applyStagedLocked(component)
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
	// update-service would refuse it anyway. A staged transfer skips this:
	// staging the running version's own image is exactly what a delta chain
	// needs as its base, and nothing is installed until the phone asks for it.
	if !start.Staged {
		if target := versionFromBundleID(start.BundleID); target != "" {
			if installed := r.installedVersion(start.Component); sameVersion(target, installed) {
				r.log.Warnf("OTA: %s is already installed on %s, refusing START", target, componentName(start.Component))
				r.send(EncodeStartAck(StartAlreadyInstalled, 0, windowChunks, ackEvery, MaxChunkSize))
				return
			}
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
	mode := "transfer"
	if start.Staged {
		mode = "staged transfer"
	}
	r.log.Infof("OTA: %s of %s (%s, %d bytes) starting at offset %d",
		mode, start.BundleID, componentName(start.Component), start.TotalSize, resume)
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

	if start.Staged {
		r.finishStagedTransferLocked(start, finalPath)
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
	r.installStaged = false
	r.installQueuedAt = time.Now().Unix()
	r.sess = nil
	r.lastPhase = PhaseInstalling
	r.lastPercent = 0
	r.publishStatusLocked("installing")
	r.startInstallWatcherLocked()
	r.armInstallFeedbackLocked()
	r.armInhibitorTimeoutLocked()
	r.send(EncodeCompleteAck(CompleteOK))
}

// finishStagedTransferLocked ends a verified staged transfer: the artifact
// stays in the component's update directory (or is copied to the board that
// owns it) and nothing is installed. There is no update-service feedback to
// wait for, so the receiver returns to idle right away and the phone learns
// what it staged from COMPLETE_ACK.
func (r *Receiver) finishStagedTransferLocked(start Start, finalPath string) {
	// The sidecar tracks a partial transfer; a staged artifact is complete and
	// is now owned by update-service's retention, same as an installed full
	// image. The .part file is gone either way.
	r.staging.Forget(start.Component, start.BundleID)

	if stager, ok := r.inst[start.Component].(Stager); ok {
		if err := stager.Stage(start.Component, finalPath); err != nil {
			r.log.Errorf("OTA: staging %s for %s failed: %v", start.BundleID, componentName(start.Component), err)
			r.send(EncodeCompleteAck(CompleteQueueFailed))
			r.releaseSessionLocked()
			return
		}
	}

	r.log.Infof("OTA: %s staged for %s", start.BundleID, componentName(start.Component))
	r.send(EncodeCompleteAck(CompleteOK))
	r.releaseSessionLocked()
}

// applyStagedLocked queues an install of everything staged in a component's
// update directory and starts watching update-service, exactly like a normal
// transfer's install phase.
func (r *Receiver) applyStagedLocked(component byte) {
	applier, ok := r.inst[component].(StagedApplier)
	if !ok {
		r.send(EncodeError(ErrInternal, "staged installs unsupported"))
		return
	}
	if err := applier.ApplyStaged(component); err != nil {
		r.log.Errorf("OTA: queueing staged install for %s failed: %v", componentName(component), err)
		r.send(EncodeError(ErrInternal, "could not start the install"))
		return
	}

	// A single artifact's bundle ID is not known here; the phone identifies the
	// operation by the component.
	r.state = stateInstalling
	r.installComp = component
	r.installBundleID = ""
	r.installStaged = true
	r.installQueuedAt = time.Now().Unix()
	r.sess = nil
	r.lastPhase = PhaseInstalling
	r.lastPercent = 0
	r.publishStatusLocked("installing")
	r.startInstallWatcherLocked()
	r.armInstallFeedbackLocked()
	r.armInhibitorTimeoutLocked()
	r.setInhibitorLocked("BLE staged install")
	r.send(EncodeInstallProgress(PhaseInstalling, 0, "applying staged updates"))
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
	if r.feedbackTmr != nil {
		r.feedbackTmr.Stop()
		r.feedbackTmr = nil
	}
}

// armInstallFeedbackLocked (re)starts the silence watchdog for a queued
// install. Every field update-service writes for this component re-arms it,
// including its 30-second heartbeat, so it only fires when update-service
// produced no sign of life at all.
func (r *Receiver) armInstallFeedbackLocked() {
	if r.feedbackTmr != nil {
		r.feedbackTmr.Stop()
	}
	comp := r.installComp
	r.feedbackTmr = time.AfterFunc(installFeedbackTimeout, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.state != stateInstalling || r.installComp != comp {
			return
		}
		r.log.Warnf("OTA: update-service reported nothing for %s within %s; the install did not start",
			componentName(comp), installFeedbackTimeout)
		r.lastPhase = PhaseFailure
		r.lastPercent = 0
		r.send(EncodeInstallProgress(PhaseFailure, 0, "the scooter did not start the installation"))
		r.finishInstallLocked()
	})
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

	// Any field for this component proves update-service is alive; only this
	// component's fields count, so an unrelated DBC update cannot mask a dead
	// MDB install.
	if r.state == stateInstalling && strings.HasSuffix(field, ":"+comp) {
		r.armInstallFeedbackLocked()
	}

	switch field {
	case handoffFieldPrefix + comp:
		if r.waitingOutcome {
			return
		}
		// A cross-board handoff (dashboard boot, artifact copy) is update-service
		// work in all but name; relay it as the install's current activity.
		r.lastPhase = PhaseInstalling
		r.sendProgressPacedLocked(strings.TrimSpace(value))
	case "install-refused:" + comp:
		if r.waitingOutcome {
			return
		}
		// update-service dropped the request without touching its status
		// (another operation holds the updater, or the commit gate is open).
		// Report it here so the phone does not sit in "installing" until the
		// watchdog expires. The timestamp keeps a refusal written before this
		// install was queued from failing it.
		ts, reason, ok := parseInstallRefusal(value)
		if !ok || ts < r.installQueuedAt {
			return
		}
		r.log.Warnf("OTA: update-service refused the %s install: %s", comp, reason)
		r.lastPhase = PhaseFailure
		r.lastPercent = 0
		r.send(EncodeInstallProgress(PhaseFailure, 0, reason))
		r.finishInstallLocked()
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
		case "staged-noop":
			if r.waitingOutcome {
				return
			}
			// update-service resolved the staged set to nothing applicable, so
			// nothing is installed and nothing reboots. That status never clears
			// itself, so this is where the receiver stops waiting for it.
			r.log.Infof("OTA: update-service found nothing to install for %s", comp)
			r.lastPhase, r.lastPercent = PhaseUpToDate, 100
			r.send(EncodeInstallProgress(PhaseUpToDate, 100, "nothing to install"))
			r.finishInstallLocked()
		case "error":
			r.lastPhase = PhaseFailure
			msg, _ := r.watcherFetch("error-message:" + comp)
			r.send(EncodeInstallProgress(PhaseFailure, 0, msg))
			r.finishInstallLocked()
		}
	}
}

// parseInstallRefusal decodes an install-refused:<component> value, written by
// update-service as "<unix-seconds>:<reason>".
func parseInstallRefusal(value string) (int64, string, bool) {
	sep := strings.IndexByte(value, ':')
	if sep <= 0 {
		return 0, "", false
	}
	ts, err := strconv.ParseInt(value[:sep], 10, 64)
	if err != nil {
		return 0, "", false
	}
	reason := strings.TrimSpace(value[sep+1:])
	if reason == "" {
		reason = "the scooter is busy with another update"
	}
	return ts, reason, true
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
	// to the DBC by the handoff, and delta files are consumed by
	// update-service during apply.
	installed := phase == PhasePendingReboot || phase == PhaseSuccess
	// A staged apply has no single bundle ID: its artifacts belong to
	// update-service's retention (deltas it consumed, the base it kept) and
	// must not be swept here.
	if r.installBundleID != "" {
		if installed && r.installComp == ComponentMDB && !isDelta(r.installBundleID) {
			r.staging.Forget(r.installComp, r.installBundleID)
		} else {
			r.staging.Discard(r.installComp, r.installBundleID)
		}
	}
	r.installBundleID = ""

	// The inhibitor is released below; its watchdogs must not fire later.
	if r.inhibitTmr != nil {
		r.inhibitTmr.Stop()
		r.inhibitTmr = nil
	}
	if r.feedbackTmr != nil {
		r.feedbackTmr.Stop()
		r.feedbackTmr = nil
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

	// A staged apply has no single bundle ID, so it is the one case whose
	// artifacts are not the receiver's to keep: cross-board staging copies them
	// to the board that installs them, and a repeat only needs the source. Only
	// a queued install means the dashboard has them; a staged set that turned
	// out to be up to date is still the user's only copy.
	if phase == PhasePendingReboot && r.installStaged {
		if pruner, ok := r.inst[r.installComp].(StagedPruner); ok {
			pruner.PruneStaged(r.installComp)
		}
	}
	r.installStaged = false
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
