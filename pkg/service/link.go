package service

import (
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/librescoot/bluetooth-service/pkg/ble"
	"github.com/librescoot/bluetooth-service/pkg/usock"
)

// UART link management: baud-rate negotiation with the nRF52 and automatic
// fallback to the 115200 boot baud rate. Mirrors src/link_mgmt.c in mdb-nrf52.
//
// Rules:
//   - Both sides always boot at 115200; the negotiated baud rate is never persisted.
//   - Capability discovery uses an explicit LINK_CAPS probe: legacy firmware
//     silently drops unknown USOCK service ids, so a probe timeout cleanly
//     identifies firmware without 1 Mbaud support (stay at 115200 forever).
//   - Handshake: BAUD_SET at 115200 -> nRF ACKs, drains TX, switches and arms a
//     2 s verify timer -> we reopen the port at 1 M and PING until echoed.
//   - While at 1 M, a keepalive PING runs every 5 s. Two consecutive misses or a
//     burst of framing garbage (the nRF rebooted back to 115200) trigger a
//     fallback: reopen at 115200, re-init, renegotiate.
//   - Before nRF DFU (bootloader talks 115200 only), EnsureLinkBaud(115200)
//     downshifts cooperatively, with the nRF's 15 s idle revert as the backstop.

const (
	linkBaudDefault = 115200
	linkBaudFast    = 1000000

	linkProbeTimeout   = 300 * time.Millisecond
	linkProbeRetries   = 3
	linkAckTimeout     = 300 * time.Millisecond
	linkPingTimeout    = 200 * time.Millisecond
	linkPingRetries    = 5
	keepalivePeriod    = 5 * time.Second
	keepaliveTimeout   = 2 * time.Second
	keepaliveMaxMisses = 2

	// garbage thresholds per keepalive period: a peer transmitting at the wrong
	// baud rate shows up as sync-hunt discards and header CRC failures
	garbageDiscardLimit = 16
	garbageCRCLimit     = 4

	// nRF reverts to 115200 after 15 s without a valid frame; wait one second
	// longer when the cooperative downshift fails
	nrfIdleRevertWait = 16 * time.Second
)

// LinkManager owns the negotiated baud state of the USOCK link.
type LinkManager struct {
	svc *Service

	mu          sync.Mutex
	currentBaud int
	caps        int
	opBusy      bool // a negotiation or fallback is running (owns the port)
	suspended   bool // firmware updater owns the port; refuse all operations
	keepStop    chan struct{}

	capsCh chan int
	ackCh  chan int
	pingCh chan int

	nonce int32
}

func newLinkManager(svc *Service) *LinkManager {
	return &LinkManager{
		svc:         svc,
		currentBaud: linkBaudDefault,
		capsCh:      make(chan int, 4),
		ackCh:       make(chan int, 4),
		pingCh:      make(chan int, 4),
	}
}

// resetToDefault records that the port was (re)opened at the boot baud rate
// outside the link manager's control (startNormally, ReconnectUSock) and stops
// any keepalive still supervising the previous connection.
func (lm *LinkManager) resetToDefault() {
	lm.stopKeepalive()
	lm.mu.Lock()
	lm.currentBaud = linkBaudDefault
	lm.mu.Unlock()
}

// beginOp claims the link for a negotiation or fallback. Returns false when the
// link manager is suspended (firmware update in progress) or another operation
// is already running.
func (lm *LinkManager) beginOp() bool {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	if lm.suspended || lm.opBusy {
		return false
	}
	lm.opBusy = true
	return true
}

func (lm *LinkManager) endOp() {
	lm.mu.Lock()
	lm.opBusy = false
	lm.mu.Unlock()
}

// Suspend hands exclusive port ownership to the caller (the nRF firmware
// updater): stops the keepalive, blocks new negotiations/fallbacks, and waits
// for an in-flight operation to finish. After Suspend returns, the link manager
// will not touch the serial port until Resume. Callers should follow up with
// EnsureLinkBaud(115200) to settle the wire before handing it to nrfutil.
func (lm *LinkManager) Suspend() {
	lm.mu.Lock()
	lm.suspended = true
	lm.mu.Unlock()

	lm.stopKeepalive()

	for {
		lm.mu.Lock()
		busy := lm.opBusy
		lm.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Resume lifts a Suspend. It does not start a negotiation by itself — the
// updater's ReconnectUSock path does that after the port is re-established.
func (lm *LinkManager) Resume() {
	lm.mu.Lock()
	lm.suspended = false
	lm.mu.Unlock()
}

// CurrentBaud returns the baud rate the port is currently open at.
func (lm *LinkManager) CurrentBaud() int {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	return lm.currentBaud
}

// Caps returns the last probed capability bitmask (0 = legacy firmware).
func (lm *LinkManager) Caps() int {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	return lm.caps
}

// HandleLinkMessage consumes decoded LINK-service responses from the nRF.
// Called from HandleUSockMessage.
func (lm *LinkManager) HandleLinkMessage(absSubTypeKey uint16, value interface{}) {
	v, ok := convertToInt(value)
	if !ok {
		lm.svc.log.Warnf("Link: non-integer value for subtype 0x%04x: %v", absSubTypeKey, value)
		return
	}

	switch ble.SubType(absSubTypeKey - uint16(ble.TypeLink)) {
	case ble.TypeLinkCaps:
		select {
		case lm.capsCh <- v:
		default:
		}
	case ble.TypeLinkBaudSet:
		select {
		case lm.ackCh <- v:
		default:
		}
	case ble.TypeLinkPing:
		select {
		case lm.pingCh <- v:
		default:
		}
	case ble.TypeLinkStats:
		// stats arrive as an array and are handled in HandleUSockMessage's
		// generic logging; nothing to correlate here
	default:
		lm.svc.log.Debugf("Link: unhandled subtype 0x%04x = %d", absSubTypeKey, v)
	}
}

// StartNegotiation kicks off capability probing and (if supported) the upshift
// to 1 Mbaud. Non-blocking; concurrent calls collapse into one negotiation.
func (lm *LinkManager) StartNegotiation() {
	go lm.negotiate()
}

func (lm *LinkManager) negotiate() {
	if !lm.beginOp() {
		return
	}
	defer lm.endOp()

	lm.mu.Lock()
	alreadyFast := lm.currentBaud != linkBaudDefault
	lm.mu.Unlock()
	if alreadyFast {
		// negotiation always starts from the boot baud rate; if we are already
		// upshifted the keepalive supervises the link
		return
	}

	lm.stopKeepalive()

	caps, ok := lm.probeCaps()
	lm.mu.Lock()
	lm.caps = caps
	lm.mu.Unlock()

	if !ok {
		lm.svc.log.Infof("Link: no CAPS response, legacy nRF firmware; staying at %d baud", linkBaudDefault)
		lm.publishStatus()
		return
	}
	lm.svc.log.Infof("Link: nRF capabilities 0x%x", caps)

	if caps&ble.LinkCapBaud1M == 0 {
		lm.publishStatus()
		return
	}

	for attempt := 1; attempt <= 2; attempt++ {
		if err := lm.upshift(); err != nil {
			lm.svc.log.Warnf("Link: upshift attempt %d failed: %v", attempt, err)
			continue
		}
		lm.svc.log.Infof("Link: negotiated %d baud", linkBaudFast)
		lm.publishStatus()
		lm.startKeepalive()
		// upshift reopened the port; concurrent state writes may have been
		// lost mid-switch — re-push everything now that the link is stable
		lm.svc.resyncStateToNRF("baud upshift")
		return
	}

	lm.svc.log.Warnf("Link: staying at %d baud after failed negotiation", linkBaudDefault)
	lm.publishStatus()
	// the failed upshift also reopened the port (possibly twice)
	lm.svc.resyncStateToNRF("failed baud negotiation")
}

// probeCaps sends LINK_CAPS requests and waits for a response; a timeout after
// all retries means legacy firmware.
func (lm *LinkManager) probeCaps() (int, bool) {
	lm.drain(lm.capsCh)
	for i := 0; i < linkProbeRetries; i++ {
		if err := writeUARTMessage(lm.svc.GetUSock(), ble.TypeLink, ble.TypeLinkCaps, 0); err != nil {
			lm.svc.log.Warnf("Link: CAPS probe write failed: %v", err)
			return 0, false
		}
		select {
		case caps := <-lm.capsCh:
			return caps, true
		case <-time.After(linkProbeTimeout):
		}
	}
	return 0, false
}

// upshift commands the switch to 1 Mbaud and verifies it; on failure the port
// is back at 115200 when it returns.
func (lm *LinkManager) upshift() error {
	// command the switch; the nRF ACKs at the current baud, waits for its TX
	// ring to drain, then switches and arms its verify timer
	lm.drain(lm.ackCh)
	acked := false
	for i := 0; i < 2 && !acked; i++ {
		if err := writeUARTMessage32(lm.svc.GetUSock(), ble.TypeLink, ble.TypeLinkBaudSet, linkBaudFast); err != nil {
			return fmt.Errorf("BAUD_SET write: %w", err)
		}
		select {
		case v := <-lm.ackCh:
			if v != linkBaudFast {
				return fmt.Errorf("BAUD_SET rejected, nRF stays at %d", v)
			}
			acked = true
		case <-time.After(linkAckTimeout):
		}
	}
	if !acked {
		// no ACK: the nRF never committed to switching, so the link is still
		// consistent at 115200; its verify timer is not armed
		return fmt.Errorf("no BAUD_SET ACK")
	}

	// give the nRF time to drain the ACK out of its shifter and re-init its UARTE
	time.Sleep(30 * time.Millisecond)

	if err := lm.reopenAt(linkBaudFast); err != nil {
		return fmt.Errorf("reopen at %d: %w", linkBaudFast, err)
	}

	if lm.pingVerify(linkPingRetries, linkPingTimeout) {
		return nil
	}

	// wait out the nRF's verify window (2 s, checked on a 500 ms supervisor
	// tick) so it is guaranteed back at 115200 before we reopen there
	time.Sleep(3 * time.Second)
	if err := lm.reopenAt(linkBaudDefault); err != nil {
		return fmt.Errorf("reopen back at %d: %w", linkBaudDefault, err)
	}
	if !lm.pingVerify(linkPingRetries, linkPingTimeout) {
		lm.svc.log.Warnf("Link: no PING echo at %d after failed upshift", linkBaudDefault)
	}
	return fmt.Errorf("PING verify at %d failed", linkBaudFast)
}

// pingVerify sends PINGs until one is echoed or the retries run out.
func (lm *LinkManager) pingVerify(retries int, timeout time.Duration) bool {
	for i := 0; i < retries; i++ {
		if lm.ping(timeout) {
			return true
		}
	}
	return false
}

// ping sends a single PING and waits for the matching echo.
func (lm *LinkManager) ping(timeout time.Duration) bool {
	lm.drain(lm.pingCh)
	lm.nonce++
	nonce := lm.nonce
	if err := writeUARTMessage32(lm.svc.GetUSock(), ble.TypeLink, ble.TypeLinkPing, nonce); err != nil {
		return false
	}
	deadline := time.After(timeout)
	for {
		select {
		case v := <-lm.pingCh:
			if int32(v) == nonce {
				return true
			}
			// stale echo from an earlier ping; keep waiting
		case <-deadline:
			return false
		}
	}
}

// reopenAt closes the current port and opens it at the given baud rate,
// re-applying handlers. The USOCK receive state machine starts fresh.
func (lm *LinkManager) reopenAt(baud int) error {
	if err := lm.svc.reopenUSockAtBaud(baud); err != nil {
		return err
	}
	lm.mu.Lock()
	lm.currentBaud = baud
	lm.mu.Unlock()
	return nil
}

// EnsureLinkBaud brings the link to the requested baud rate, cooperatively if
// possible. Used before nRF DFU (bootloader talks 115200 only). Blocks until
// the port is reopened at the requested rate.
func (lm *LinkManager) EnsureLinkBaud(baud int) error {
	lm.mu.Lock()
	current := lm.currentBaud
	lm.mu.Unlock()
	if current == baud {
		return nil
	}

	lm.stopKeepalive()

	lm.svc.log.Infof("Link: downshifting %d -> %d baud", current, baud)
	lm.drain(lm.ackCh)
	cooperative := false
	if err := writeUARTMessage32(lm.svc.GetUSock(), ble.TypeLink, ble.TypeLinkBaudSet, int32(baud)); err == nil {
		select {
		case v := <-lm.ackCh:
			cooperative = v == baud
		case <-time.After(linkAckTimeout):
		}
	}

	if !cooperative {
		// the nRF did not acknowledge; wait out its idle-revert window so it is
		// guaranteed to be back at 115200 before nrfutil takes the port
		lm.svc.log.Warnf("Link: cooperative downshift failed, waiting %s for nRF idle revert", nrfIdleRevertWait)
		if baud == linkBaudDefault {
			time.Sleep(nrfIdleRevertWait)
		}
	} else {
		time.Sleep(30 * time.Millisecond)
	}

	if err := lm.reopenAt(baud); err != nil {
		return err
	}
	if !lm.pingVerify(linkPingRetries, linkPingTimeout) {
		lm.svc.log.Warnf("Link: no PING echo after downshift to %d (continuing)", baud)
	}
	lm.publishStatus()
	return nil
}

// keepalive supervises the fast link: PING every period, fall back to 115200 on
// consecutive misses or a garbage storm.
func (lm *LinkManager) startKeepalive() {
	lm.mu.Lock()
	if lm.keepStop != nil {
		lm.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	lm.keepStop = stop
	lm.mu.Unlock()

	go lm.keepaliveLoop(stop)
}

func (lm *LinkManager) stopKeepalive() {
	lm.mu.Lock()
	if lm.keepStop != nil {
		close(lm.keepStop)
		lm.keepStop = nil
	}
	lm.mu.Unlock()
}

func (lm *LinkManager) keepaliveLoop(stop chan struct{}) {
	misses := 0
	var lastStats usock.Stats
	if sock := lm.svc.getUSockConcrete(); sock != nil {
		lastStats = sock.GetStats()
	}

	ticker := time.NewTicker(keepalivePeriod)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}

		ok := lm.ping(keepaliveTimeout)

		garbage := false
		if sock := lm.svc.getUSockConcrete(); sock != nil {
			stats := sock.GetStats()
			if stats.SyncDiscards-lastStats.SyncDiscards >= garbageDiscardLimit ||
				stats.HeaderCRCFails-lastStats.HeaderCRCFails >= garbageCRCLimit {
				garbage = true
			}
			lastStats = stats
		}

		if ok && !garbage {
			misses = 0
			continue
		}
		if !ok {
			misses++
		}

		if misses >= keepaliveMaxMisses || (garbage && !ok) {
			lm.svc.log.Warnf("Link: keepalive lost at %d baud (misses=%d, garbage=%v), falling back to %d",
				linkBaudFast, misses, garbage, linkBaudDefault)
			// detach from the stop channel before fallback: negotiate() restarts
			// a fresh keepalive on success
			lm.mu.Lock()
			if lm.keepStop != nil {
				close(lm.keepStop)
				lm.keepStop = nil
			}
			lm.mu.Unlock()

			lm.fallback()
			return
		}
	}
}

// fallback reopens at the boot baud rate, re-initializes the nRF (it most
// likely rebooted) and renegotiates. Refused while suspended: during a firmware
// update the "dead" link is expected (nrfutil owns the port), and reopening it
// here would steal the DFU traffic.
func (lm *LinkManager) fallback() {
	if !lm.beginOp() {
		lm.svc.log.Infof("Link: fallback skipped (link manager suspended or busy)")
		return
	}

	if err := lm.reopenAt(linkBaudDefault); err != nil {
		lm.svc.log.Errorf("Link: fallback reopen failed: %v", err)
		lm.endOp()
		return
	}
	lm.publishStatus()

	if err := lm.svc.InitializeNRF52(); err != nil {
		lm.svc.log.Errorf("Link: re-initialization after fallback failed: %v", err)
	}
	lm.endOp()
	lm.StartNegotiation()
}

func (lm *LinkManager) publishStatus() {
	lm.mu.Lock()
	baud := lm.currentBaud
	caps := lm.caps
	lm.mu.Unlock()

	if err := lm.svc.ipc.Hash("ble").SetMany(map[string]any{
		"link-baud": strconv.Itoa(baud),
		"link-caps": strconv.Itoa(caps),
	}); err != nil {
		lm.svc.log.Errorf("Link: failed to publish link status: %v", err)
	}
}

func (lm *LinkManager) drain(ch chan int) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}
