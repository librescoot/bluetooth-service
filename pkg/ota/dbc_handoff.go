package ota

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
// On failure the lock is released (complete-dbc) and an error is written to
// the "ota" hash so the phone gets a failure notification through the same
// relay as real install errors.
type DBCInstaller struct {
	IPC *ipc.Client
	Log *logger.Logger
}

const (
	dbcAddr           = "192.168.7.2"
	dbcSSHPort        = 22
	dbcDataServerPort = 8080
	dbcOtaDir         = "/data/ota/dbc"

	dbcBootTimeout       = 3 * time.Minute
	dbcHeartbeatInterval = 5 * time.Minute
	dbcTransferTimeout   = 10 * time.Minute
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

func (di *DBCInstaller) run(path string) {
	if err := di.deliver(path); err != nil {
		di.Log.Errorf("OTA: DBC handoff failed: %v", err)
		// release the update lock (we never queued the install)
		if _, err := di.IPC.LPush("scooter:update", "complete-dbc"); err != nil {
			di.Log.Warnf("OTA: releasing DBC update lock failed: %v", err)
		}
		// surface the failure through the same "ota" hash the receiver's
		// install watcher already relays to the phone
		if err := di.IPC.Hash(installHash).SetMany(map[string]any{
			"error-message:dbc": "handoff: " + err.Error(),
			"status:dbc":        "error",
		}); err != nil {
			di.Log.Warnf("OTA: reporting DBC handoff failure failed: %v", err)
		}
	}
}

func (di *DBCInstaller) deliver(path string) error {
	// claim the DBC update lock; idempotent, doubles as the heartbeat message
	if _, err := di.IPC.LPush("scooter:update", "start-dbc"); err != nil {
		return fmt.Errorf("claiming DBC update lock: %w", err)
	}

	stopHeartbeat := make(chan struct{})
	defer close(stopHeartbeat)
	go func() {
		ticker := time.NewTicker(dbcHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stopHeartbeat:
				return
			case <-ticker.C:
				if _, err := di.IPC.LPush("scooter:update", "start-dbc"); err != nil {
					di.Log.Warnf("OTA: DBC lock heartbeat failed: %v", err)
				}
			}
		}
	}()

	di.Log.Infof("OTA: waiting for DBC to become reachable...")
	if err := di.waitReachable(dbcBootTimeout); err != nil {
		return err
	}

	remotePath := filepath.Join(dbcOtaDir, filepath.Base(path))
	di.Log.Infof("OTA: transferring %s to DBC:%s", filepath.Base(path), remotePath)
	if err := di.putToDataServer(path, remotePath); err != nil {
		di.Log.Warnf("OTA: data-server PUT failed (%v), falling back to scp", err)
		if err := di.scpTransfer(path, remotePath); err != nil {
			return fmt.Errorf("transfer to DBC failed: %w", err)
		}
	}

	if _, err := di.IPC.LPush("scooter:update:dbc", "update-from-file:"+remotePath); err != nil {
		return fmt.Errorf("queueing DBC install: %w", err)
	}

	// success: leave the lock held for update-service's own
	// start-dbc/complete-dbc cycle (heartbeat stops via defer)
	di.Log.Infof("OTA: DBC install queued (%s)", remotePath)
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
