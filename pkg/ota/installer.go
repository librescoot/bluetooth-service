package ota

import (
	"fmt"

	ipc "github.com/librescoot/redis-ipc"
)

// MenderInstaller queues completed bundles for the local (MDB) update-service
// instance via its Redis request list; update-service then runs
// `mender-update install` and reports progress in the "ota" hash.
type MenderInstaller struct {
	IPC *ipc.Client
}

func (mi *MenderInstaller) Install(component byte, path string) error {
	if component != ComponentMDB {
		return fmt.Errorf("mender installer only handles the mdb component")
	}
	return ipc.SendRequest(mi.IPC, "scooter:update:mdb", "update-from-file:"+path)
}

// Stage is a no-op for the MDB: its update directory is /data/ota/mdb, which is
// where the receiver's staging already puts a completed artifact, and that is
// the directory update-service installs from. Nothing has to move.
func (mi *MenderInstaller) Stage(component byte, path string) error {
	if component != ComponentMDB {
		return fmt.Errorf("mender installer only handles the mdb component")
	}
	return nil
}

// ApplyStaged tells the MDB's update-service to install whatever is staged in
// /data/ota/mdb, the same command UMS sends after filling the directory.
func (mi *MenderInstaller) ApplyStaged(component byte) error {
	if component != ComponentMDB {
		return fmt.Errorf("mender installer only handles the mdb component")
	}
	return ipc.SendRequest(mi.IPC, "scooter:update:mdb", "apply-staged-updates")
}
