package ota

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	ipc "github.com/librescoot/redis-ipc"

	"github.com/librescoot/bluetooth-service/pkg/logger"
	dataupload "github.com/librescoot/data-server/client"
)

// DBCInstaller delivers a completed bundle to the dashboard controller (DBC)
// and queues it with the DBC's update-service instance. It follows the proven
// ums-service pattern (ums-service/pkg/dbc, pkg/update/loader.go):
//
//  1. LPush "start-dbc" to scooter:update — vehicle-service claims the DBC
//     update lock: forces dashboard power on, installs a suspend-only
//     inhibitor, and arms a watchdog that we feed with periodic heartbeats.
//  2. Wait until the DBC is reachable (it may have to boot first).
//  3. Transfer the bundle with the shared resumable HTTP client to
//     librescoot-data-server on the DBC (paths are relative to /data).
//  4. LPush "update-from-file:<dbc-local path>" to scooter:update:dbc — the
//     DBC's update-service (connected to the MDB's Redis) runs the install
//     and reports progress into the shared "ota" hash under :dbc fields,
//     which the OTA receiver's install watcher relays to the phone.
//  5. Stop heartbeating WITHOUT sending complete-dbc: update-service owns its
//     own start-dbc/complete-dbc cycle around the install, and releasing the
//     lock here would let the vehicle FSM cut DBC power mid-handoff.
//
// Staged transfers do not install anything: the artifact stays in the MDB's
// /data/ota/ble/dbc directory until the phone asks for the staged set to be
// applied, at which point only artifacts marked by this receiver are copied to
// the DBC in one pass and installed through its `apply-staged-updates`.
//
// On failure the lock is released (complete-dbc) and an error is written to
// the "ota" hash so the phone gets a failure notification through the same
// relay as real install errors.
type DBCInstaller struct {
	IPC *ipc.Client
	Log *logger.Logger

	// StagingDir is the root passed to the receiver. DBC artifacts live in its
	// isolated ble/dbc subdirectory; Defaults to DefaultStagingDir.
	StagingDir string
}

const (
	dbcAddr           = "192.168.7.2"
	dbcDataServerPort = 8080
	dbcOtaDir         = "/data/ota/dbc"

	dbcBootTimeout       = 3 * time.Minute
	dbcHeartbeatInterval = 5 * time.Minute
	stagedMarkerSuffix   = ".ble-staged"

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

// Stage marks the completed BLE artifact so unrelated or legacy files are not
// copied to the dashboard with this batch.
func (di *DBCInstaller) Stage(component byte, path string) error {
	if component != ComponentDBC {
		return fmt.Errorf("DBC installer only handles the dbc component")
	}
	cleanPath := filepath.Clean(path)
	if filepath.Dir(cleanPath) != filepath.Clean(di.dbcStagingDir()) || !isInstallableArtifact(cleanPath) {
		return fmt.Errorf("staged artifact is outside the DBC staging directory: %s", path)
	}
	if info, err := os.Stat(cleanPath); err != nil || info.IsDir() {
		if err != nil {
			return fmt.Errorf("staged artifact is unavailable: %w", err)
		}
		return fmt.Errorf("staged artifact is a directory: %s", path)
	}
	marker := cleanPath + stagedMarkerSuffix
	if err := os.WriteFile(marker, []byte(filepath.Base(cleanPath)+"\n"), 0o644); err != nil {
		return fmt.Errorf("marking staged DBC artifact: %w", err)
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
	return filepath.Join(di.stagingDir(), "ble", "dbc")
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
	// Remove both the isolated BLE batch and artifacts left in the old MDB-side
	// /data/ota/dbc directory. The DBC's own updater retains its base on the DBC.
	for _, dir := range []string{di.dbcStagingDir(), filepath.Join(di.stagingDir(), "dbc")} {
		entries, err := os.ReadDir(dir)
		if err != nil && !os.IsNotExist(err) {
			di.Log.Warnf("OTA: listing %s for cleanup failed: %v", dir, err)
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || (!isInstallableArtifact(name) && !strings.HasSuffix(name, stagedMarkerSuffix)) {
				continue
			}
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
				di.Log.Warnf("OTA: removing %s after the dashboard install: %v", filepath.Join(dir, name), err)
			}
		}
	}
}

// isInstallableArtifact reports whether name is a supported full image or delta.
func isInstallableArtifact(path string) bool {
	return strings.HasSuffix(path, ".mender") || strings.HasSuffix(path, ".delta") ||
		strings.HasSuffix(path, ".hdelta")
}

// stagedArtifacts lists only bundles received as BLE staged transfers. Files
// without a marker are not part of the phone's current staged set.
func (di *DBCInstaller) stagedArtifacts() ([]string, error) {
	dir := di.dbcStagingDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading staged DBC artifacts: %w", err)
	}
	var sources []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, stagedMarkerSuffix) {
			continue
		}
		artifact := strings.TrimSuffix(name, stagedMarkerSuffix)
		if !isInstallableArtifact(artifact) {
			continue
		}
		path := filepath.Join(dir, artifact)
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		sources = append(sources, path)
	}
	return sources, nil
}

func (di *DBCInstaller) copyToDBC(localPath, remotePath string) error {
	return di.putToDataServer(localPath, remotePath)
}

func (di *DBCInstaller) waitReachable(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", dbcAddr, dbcDataServerPort), 2*time.Second)
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

// putToDataServer uploads through the shared client. Each chunk has its own
// timeout and retry budget; there is no transfer-wide deadline or SCP restart.
func (di *DBCInstaller) putToDataServer(localPath, remotePath string) error {
	urlPath := strings.TrimPrefix(remotePath, "/data")
	if urlPath == "" || urlPath[0] != '/' {
		return fmt.Errorf("remote path must be under /data, got %q", remotePath)
	}
	url := fmt.Sprintf("http://%s:%d%s", dbcAddr, dbcDataServerPort, urlPath)
	return dataupload.UploadFile(context.Background(), url, localPath, dataupload.Options{
		RequestTimeout: 2 * time.Minute,
	})
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
