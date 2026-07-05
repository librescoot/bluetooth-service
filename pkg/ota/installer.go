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
