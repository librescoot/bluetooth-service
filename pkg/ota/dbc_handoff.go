package ota

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	ipc "github.com/librescoot/redis-ipc"

	"github.com/librescoot/bluetooth-service/pkg/logger"
)

// DBCInstaller delivers a completed bundle to the dashboard controller (DBC)
// and queues it with the DBC's update-service instance. It follows the proven
// ums-service pattern (ums-service/pkg/dbc, pkg/update/loader.go):
//
//  1. LPush "start-dbc" to scooter:update — vehicle-service claims the DBC
//     update lock: forces dashboard power on, installs a suspend-only
//     inhibitor, and arms a watchdog that we feed with periodic heartbeats.
//  2. Wait until the DBC is reachable (it may have to boot first).
//  3. Transfer the bundle: HTTP PUT to librescoot-data-server on the DBC
//     (paths are relative to its /data dir), falling back to scp.
//  4. LPush "update-from-file:<dbc-local path>" to scooter:update:dbc — the
//     DBC's update-service (connected to the MDB's Redis) runs the install
//     and reports progress into the shared "ota" hash under :dbc fields,
//     which the OTA receiver's install watcher relays to the phone.
//  5. Stop heartbeating WITHOUT sending complete-dbc: update-service owns its
//     own start-dbc/complete-dbc cycle around the install, and releasing the
//     lock here would let the vehicle FSM cut DBC power mid-handoff.
//
// Staged transfers do not install anything: the artifact stays in the MDB's
// /data/ota/dbc directory until the phone asks for the staged set to be
// applied, at which point every staged artifact is copied to the DBC in one
// pass and then installed through the DBC's own `apply-staged-updates`.
//
// On failure the lock is released (complete-dbc) and an error is written to
// the "ota" hash so the phone gets a failure notification through the same
// relay as real install errors.
type DBCInstaller struct {
	IPC *ipc.Client
	Log *logger.Logger

	// StagingDir is the MDB directory holding artifacts addressed to the DBC,
	// the same directory the receiver stages into. Defaults to
	// DefaultStagingDir's dbc subdirectory.
	StagingDir string
}

const (
	dbcAddr           = "192.168.7.2"
	dbcSSHPort        = 22
	dbcDataServerPort = 8080
	dbcOtaDir         = "/data/ota/dbc"

	dbcBootTimeout       = 3 * time.Minute
	dbcHeartbeatInterval = 5 * time.Minute
	dbcTransferTimeout   = 10 * time.Minute

	// handoffInterval is how often an in-flight handoff republishes its
	// liveness field. It is well under the receiver's install-feedback
	// watchdog, which re-arms on every field published for the component, so a
	// dashboard that takes minutes to boot is not mistaken for a dead updater.
	handoffInterval = 20 * time.Second
)

// Install queues the handoff and returns immediately: powering and booting the
// DBC takes minutes, and Install is called from the USOCK read loop.
func (di *DBCInstaller) Install(component byte, path string) error {
	if component != ComponentDBC {
		return fmt.Errorf("DBC installer only handles the dbc component")
	}
	go di.run(path)
	return nil
}

// Stage is a no-op for the DBC: the artifact already sits in the MDB's
// /data/ota/dbc directory, which is where staged DBC artifacts wait. Copying it
// to the dashboard happens once, when the whole staged set is applied.
func (di *DBCInstaller) Stage(component byte, path string) error {
	if component != ComponentDBC {
		return fmt.Errorf("DBC installer only handles the dbc component")
	}
	return nil
}

// ApplyStaged copies every staged DBC artifact to the dashboard and asks its
// update-service to install them. Asynchronous like Install.
func (di *DBCInstaller) ApplyStaged(component byte) error {
	if component != ComponentDBC {
		return fmt.Errorf("DBC installer only handles the dbc component")
	}
	go di.runApplyStaged()
	return nil
}

func (di *DBCInstaller) stagingDir() string {
	if di.StagingDir != "" {
		return di.StagingDir
	}
	return DefaultStagingDir
}

func (di *DBCInstaller) dbcStagingDir() string {
	return filepath.Join(di.stagingDir(), "dbc")
}

func (di *DBCInstaller) run(path string) {
	if err := di.deliver(path); err != nil {
		di.Log.Errorf("OTA: DBC handoff failed: %v", err)
		di.reportFailure(err)
	}
}

func (di *DBCInstaller) runApplyStaged() {
	if err := di.deliverStaged(); err != nil {
		di.Log.Errorf("OTA: DBC staged install failed: %v", err)
		di.reportFailure(err)
	}
}

// reportFailure releases the update lock (we never queued the install) and
// surfaces the failure through the same "ota" hash the receiver's install
// watcher already relays to the phone.
func (di *DBCInstaller) reportFailure(err error) {
	if _, pushErr := di.IPC.LPush("scooter:update", "complete-dbc"); pushErr != nil {
		di.Log.Warnf("OTA: releasing DBC update lock failed: %v", pushErr)
	}
	if setErr := di.IPC.Hash(installHash).SetMany(map[string]any{
		"error-message:dbc": "handoff: " + err.Error(),
		"status:dbc":        "error",
	}); setErr != nil {
		di.Log.Warnf("OTA: reporting DBC handoff failure failed: %v", setErr)
	}
}

func (di *DBCInstaller) deliver(path string) error {
	live := di.newLiveness("waiting for the dashboard")
	defer live.Stop()

	di.Log.Infof("OTA: waiting for DBC to become reachable...")
	if err := di.waitReachable(dbcBootTimeout); err != nil {
		return err
	}

	remotePath := filepath.Join(dbcOtaDir, filepath.Base(path))
	live.Set("copying update to the dashboard")
	di.Log.Infof("OTA: transferring %s to DBC:%s", filepath.Base(path), remotePath)
	if err := di.copyToDBC(path, remotePath); err != nil {
		return fmt.Errorf("transfer to DBC failed: %w", err)
	}

	if _, err := di.IPC.LPush("scooter:update:dbc", "update-from-file:"+remotePath); err != nil {
		return fmt.Errorf("queueing DBC install: %w", err)
	}

	// success: leave the lock held for update-service's own
	// start-dbc/complete-dbc cycle (liveness stops via defer)
	di.Log.Infof("OTA: DBC install queued (%s)", remotePath)
	return nil
}

// deliverStaged copies every staged artifact to the dashboard and queues one
// staged install for the set, so the dashboard resolves and applies what
// belongs together — a delta chain — exactly as the MDB does.
func (di *DBCInstaller) deliverStaged() error {
	live := di.newLiveness("waiting for the dashboard")
	defer live.Stop()

	sources, err := di.stagedArtifacts()
	if err != nil {
		return err
	}
	if len(sources) == 0 {
		return fmt.Errorf("no staged artifacts for the dashboard")
	}

	di.Log.Infof("OTA: waiting for DBC to become reachable (%d staged artifact(s))...", len(sources))
	if err := di.waitReachable(dbcBootTimeout); err != nil {
		return err
	}

	live.Set("copying staged updates to the dashboard")
	for _, source := range sources {
		remotePath := filepath.Join(dbcOtaDir, filepath.Base(source))
		di.Log.Infof("OTA: transferring %s to DBC:%s", filepath.Base(source), remotePath)
		if err := di.copyToDBC(source, remotePath); err != nil {
			return fmt.Errorf("transfer of %s to DBC failed: %w", filepath.Base(source), err)
		}
	}

	if _, err := di.IPC.LPush("scooter:update:dbc", "apply-staged-updates"); err != nil {
		return fmt.Errorf("queueing DBC staged install: %w", err)
	}

	di.Log.Infof("OTA: DBC staged install queued (%d artifact(s))", len(sources))
	return nil
}

// PruneStaged drops the MDB's copies of artifacts the dashboard has taken over.
// They are only transport for the transfer to the dashboard: once it has them
// and its own update-service owns them, keeping a second copy of a rootfs image
// on the scooter's data partition is just wasted space. A failed install keeps
// them, so the same staged set can be applied again without re-uploading.
func (di *DBCInstaller) PruneStaged(component byte) {
	if component != ComponentDBC {
		return
	}
	sources, err := di.stagedArtifacts()
	if err != nil {
		di.Log.Warnf("OTA: listing staged DBC artifacts for cleanup failed: %v", err)
		return
	}
	for _, source := range sources {
		if err := os.Remove(source); err != nil {
			di.Log.Warnf("OTA: removing %s after the dashboard install failed: %v", source, err)
		}
	}
}

// stagedArtifacts lists the installable artifacts waiting for the dashboard.
// Partial transfers and dot files are not artifacts.
func (di *DBCInstaller) stagedArtifacts() ([]string, error) {
	entries, err := os.ReadDir(di.dbcStagingDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading staged DBC artifacts: %w", err)
	}
	var sources []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		if !strings.HasSuffix(name, ".mender") && !strings.HasSuffix(name, ".delta") &&
			!strings.HasSuffix(name, ".hdelta") {
			continue
		}
		sources = append(sources, filepath.Join(di.dbcStagingDir(), name))
	}
	return sources, nil
}

func (di *DBCInstaller) copyToDBC(localPath, remotePath string) error {
	if err := di.putToDataServer(localPath, remotePath); err != nil {
		di.Log.Warnf("OTA: data-server PUT failed (%v), falling back to scp", err)
		return di.scpTransfer(localPath, remotePath)
	}
	return nil
}

func (di *DBCInstaller) waitReachable(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", dbcAddr, dbcSSHPort), 2*time.Second)
		if err == nil {
			// This is a reachability probe; the connection carries no data,
			// so a close failure has nothing left to report.
			_ = conn.Close()
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("DBC not reachable within %s", timeout)
}

// putToDataServer uploads via librescoot-data-server, which joins request
// paths under its /data directory (PUT /ota/dbc/x writes /data/ota/dbc/x).
func (di *DBCInstaller) putToDataServer(localPath, remotePath string) error {
	urlPath := strings.TrimPrefix(remotePath, "/data")
	if urlPath == "" || urlPath[0] != '/' {
		return fmt.Errorf("remote path must be under /data, got %q", remotePath)
	}

	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}

	url := fmt.Sprintf("http://%s:%d%s", dbcAddr, dbcDataServerPort, urlPath)
	req, err := http.NewRequest(http.MethodPut, url, f)
	if err != nil {
		return err
	}
	req.ContentLength = st.Size()
	req.Header.Set("Content-Type", "application/octet-stream")

	client := &http.Client{Timeout: dbcTransferTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("PUT %s: HTTP %d", url, resp.StatusCode)
	}
	return nil
}

// scpTransfer is the fallback for images without librescoot-data-server. The
// remote mkdir uses `ssh -y` (dropbear auto-accepts unknown host keys).
func (di *DBCInstaller) scpTransfer(localPath, remotePath string) error {
	mkdir := exec.Command("ssh", "-y", "root@"+dbcAddr, "mkdir -p "+filepath.Dir(remotePath))
	if out, err := mkdir.CombinedOutput(); err != nil {
		return fmt.Errorf("remote mkdir: %v (%s)", err, strings.TrimSpace(string(out)))
	}

	scp := exec.Command("scp",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		localPath,
		fmt.Sprintf("root@%s:%s", dbcAddr, remotePath))
	if out, err := scp.CombinedOutput(); err != nil {
		return fmt.Errorf("scp: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// handoffLiveness holds the DBC update lock and keeps the "ota" hash showing
// that the handoff is alive. vehicle-service drops the lock if `start-dbc`
// stops arriving, so the lock heartbeat is not just cosmetic; handoff:<dbc> is
// what keeps the receiver from timing the handoff out and shows the phone what
// the scooter is waiting for.
type handoffLiveness struct {
	ipc *ipc.Client
	log *logger.Logger

	done    chan struct{}
	mu      sync.Mutex
	message string
	stopped bool
}

func (di *DBCInstaller) newLiveness(message string) *handoffLiveness {
	live := &handoffLiveness{ipc: di.IPC, log: di.Log, done: make(chan struct{}), message: message}
	// claim the DBC update lock; idempotent, doubles as the heartbeat
	if _, err := live.ipc.LPush("scooter:update", "start-dbc"); err != nil {
		live.log.Warnf("OTA: claiming DBC update lock failed: %v", err)
	}
	live.publish()
	go func() {
		lock := time.NewTicker(dbcHeartbeatInterval)
		handoff := time.NewTicker(handoffInterval)
		defer lock.Stop()
		defer handoff.Stop()
		for {
			select {
			case <-live.done:
				return
			case <-lock.C:
				if _, err := live.ipc.LPush("scooter:update", "start-dbc"); err != nil {
					live.log.Warnf("OTA: DBC lock heartbeat failed: %v", err)
				}
			case <-handoff.C:
				live.publish()
			}
		}
	}()
	return live
}

// Stop ends the lock heartbeat and silences the liveness field. It does not
// release the DBC update lock: update-service takes over the lock cycle for the
// install it was just asked to run, and releasing it here would let the vehicle
// FSM cut dashboard power mid-install.
func (l *handoffLiveness) Stop() {
	l.mu.Lock()
	if l.stopped {
		l.mu.Unlock()
		return
	}
	l.stopped = true
	l.mu.Unlock()
	close(l.done)
}

func (l *handoffLiveness) Set(message string) {
	l.mu.Lock()
	l.message = message
	l.mu.Unlock()
	l.publish()
}

// publish writes the liveness field the receiver watches. Best effort: losing
// one field only costs the phone a progress line, and the next tick rewrites it.
func (l *handoffLiveness) publish() {
	l.mu.Lock()
	message, stopped := l.message, l.stopped
	l.mu.Unlock()
	if stopped {
		return
	}
	if err := l.ipc.Hash(installHash).Set(handoffFieldPrefix+"dbc", message); err != nil {
		l.log.Debugf("OTA: publishing handoff liveness failed: %v", err)
	}
}
