package service

import (
	"net"
	"strconv"
	"testing"

	"github.com/alicebob/miniredis/v2"
	ipc "github.com/librescoot/redis-ipc"

	"github.com/librescoot/bluetooth-service/pkg/ble"
	"github.com/librescoot/bluetooth-service/pkg/logger"
)

func newVersionPushService(t *testing.T) (*Service, *miniredis.Miniredis) {
	t.Helper()

	mr := miniredis.RunT(t)
	host, portStr, err := net.SplitHostPort(mr.Addr())
	if err != nil {
		t.Fatalf("split miniredis addr: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse miniredis port: %v", err)
	}
	client, err := ipc.New(ipc.WithAddress(host), ipc.WithPort(port))
	if err != nil {
		t.Fatalf("ipc.New: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	return &Service{
		ipc:   client,
		usock: &mockUSOCK{},
		log:   logger.NewLogger(nil, logger.LogLevelNone),
	}, mr
}

func TestShutdownNRF52DrainsAndReplaysFinalPowerState(t *testing.T) {
	s, mr := newVersionPushService(t)
	mr.HSet("power-manager", "state", "hibernating-imminent")

	s.ShutdownNRF52()

	messages := s.usock.(*mockUSOCK).messages
	if len(messages) < 2 {
		t.Fatalf("shutdown sent %d frames, want final power state and stream disable", len(messages))
	}
	state, err := decodeCBORMessage(messages[0].data)
	if err != nil {
		t.Fatal(err)
	}
	stateKey := uint16(ble.TypePowerManagement) + uint16(ble.TypePowerManagementState)
	if got := state[uint16(ble.TypePowerManagement)][stateKey]; got != uint16(4) {
		t.Fatalf("final power state = %v, want hibernating-imminent (4)", got)
	}
	stream, err := decodeCBORMessage(messages[len(messages)-1].data)
	if err != nil {
		t.Fatal(err)
	}
	streamKey := uint16(ble.TypeDataStream) + uint16(ble.TypeDataStreamEnable)
	if got := stream[uint16(ble.TypeDataStream)][streamKey]; got != uint16(0) {
		t.Fatalf("final frame stream-enable = %v, want disabled", got)
	}
}

func versionPushFrames(t *testing.T, s *Service) []string {
	t.Helper()

	var versions []string
	for _, msg := range s.usock.(*mockUSOCK).messages {
		decoded, err := decodeCBORMessageString(msg.data)
		if err != nil {
			t.Fatalf("decode CBOR: %v", err)
		}
		key := uint16(ble.TypeScooterInfo) + uint16(ble.TypeSoftwareVersion)
		if value, ok := decoded[uint16(ble.TypeScooterInfo)][key]; ok {
			versions = append(versions, value)
		}
	}
	return versions
}

func TestPushMDBVersionSendsVersionFromVersionHash(t *testing.T) {
	t.Run("version_id", func(t *testing.T) {
		s, mr := newVersionPushService(t)
		mr.HSet("version:mdb", "version_id", "v1.15.0+20240809183558")

		if err := s.pushMDBVersion(); err != nil {
			t.Fatalf("pushMDBVersion: %v", err)
		}

		if versions := versionPushFrames(t, s); len(versions) != 1 || versions[0] != "v1.15.0+20240809183558" {
			t.Fatalf("version frames = %v, want [v1.15.0+20240809183558]", versions)
		}
	})

	t.Run("prefers version field", func(t *testing.T) {
		s, mr := newVersionPushService(t)
		mr.HSet("version:mdb", "version", "v1.15.0", "version_id", "v1.15.0+20240809183558")

		if err := s.pushMDBVersion(); err != nil {
			t.Fatalf("pushMDBVersion: %v", err)
		}

		if versions := versionPushFrames(t, s); len(versions) != 1 || versions[0] != "v1.15.0" {
			t.Fatalf("version frames = %v, want [v1.15.0]", versions)
		}
	})
}

func TestPushMDBVersionWithoutVersionWritesNothing(t *testing.T) {
	t.Run("absent hash", func(t *testing.T) {
		s, _ := newVersionPushService(t)
		if err := s.pushMDBVersion(); err == nil {
			t.Fatal("pushMDBVersion without version:mdb: want error, got nil")
		}
		if len(versionPushFrames(t, s)) != 0 {
			t.Fatal("wrote a version frame without a version")
		}
	})

	t.Run("no version field", func(t *testing.T) {
		s, mr := newVersionPushService(t)
		mr.HSet("version:mdb", "image_id", "librescoot")

		if err := s.pushMDBVersion(); err == nil {
			t.Fatal("pushMDBVersion without a version field: want error, got nil")
		}
		if len(versionPushFrames(t, s)) != 0 {
			t.Fatal("wrote a version frame without a version")
		}
	})
}
