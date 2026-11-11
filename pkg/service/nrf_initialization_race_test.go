package service

import (
	"io"
	"sync"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/librescoot/bluetooth-service/pkg/ble"
)

type initInterruptUSOCK struct {
	mu              sync.Mutex
	svc             *Service
	closeAfterHello bool
	closed          bool
	streamEnabled   bool
	streamSynced    bool
	failedWrites    int
}

func (m *initInterruptUSOCK) WriteWithFrameID(_ byte, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		m.failedWrites++
		return io.ErrClosedPipe
	}

	var message map[uint16]map[uint16]cbor.RawMessage
	if err := cbor.Unmarshal(data, &message); err != nil {
		return err
	}
	if params := message[uint16(ble.TypeBLEVersion)]; params != nil {
		key := uint16(ble.TypeBLEVersion) + uint16(ble.TypeBLEVersionHostSession)
		if raw, ok := params[key]; ok {
			var hello []int32
			if err := cbor.Unmarshal(raw, &hello); err != nil {
				return err
			}
			m.svc.handleHostSessionAck([]interface{}{hello[0], hostCapExternalTemperature, hello[2]})
			if m.closeAfterHello {
				m.closed = true
			}
		}
	}
	if params := message[uint16(ble.TypeDataStream)]; params != nil {
		enableKey := uint16(ble.TypeDataStream) + uint16(ble.TypeDataStreamEnable)
		if raw, ok := params[enableKey]; ok {
			var value uint16
			if err := cbor.Unmarshal(raw, &value); err != nil {
				return err
			}
			if value == 1 {
				m.streamEnabled = true
			}
		}
		syncKey := uint16(ble.TypeDataStream) + uint16(ble.TypeDataStreamSync)
		if _, ok := params[syncKey]; ok {
			m.streamSynced = true
		}
	}
	return nil
}

func (m *initInterruptUSOCK) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

type overlapUSOCK struct {
	svc          *Service
	helloStarted chan struct{}
	releaseHello chan struct{}
	probeStarted chan struct{}
	probeOnce    sync.Once
}

func (m *overlapUSOCK) WriteWithFrameID(_ byte, data []byte) error {
	var message map[uint16]map[uint16]cbor.RawMessage
	if err := cbor.Unmarshal(data, &message); err != nil {
		return err
	}
	if params := message[uint16(ble.TypeBLEVersion)]; params != nil {
		key := uint16(ble.TypeBLEVersion) + uint16(ble.TypeBLEVersionHostSession)
		if raw, ok := params[key]; ok {
			var hello []int32
			if err := cbor.Unmarshal(raw, &hello); err != nil {
				return err
			}
			close(m.helloStarted)
			<-m.releaseHello
			m.svc.handleHostSessionAck([]interface{}{hello[0], hostCapExternalTemperature, hello[2]})
		}
	}
	if params := message[uint16(ble.TypeLink)]; params != nil {
		key := uint16(ble.TypeLink) + uint16(ble.TypeLinkCaps)
		if _, ok := params[key]; ok {
			m.probeOnce.Do(func() { close(m.probeStarted) })
		}
	}
	return nil
}

func (m *overlapUSOCK) Close() error { return nil }

func TestRunningNotificationStartsBaudProbeDuringInitialization(t *testing.T) {
	s, mr := newVersionPushService(t)
	mr.HSet("version:mdb", "version", "v1.4.2")
	mock := &overlapUSOCK{
		svc: s, helloStarted: make(chan struct{}), releaseHello: make(chan struct{}), probeStarted: make(chan struct{}),
	}
	s.usock = mock
	s.link = newLinkManager(s)
	initDone := make(chan error, 1)
	go func() { initDone <- s.InitializeNRF52() }()

	select {
	case <-mock.helloStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("host handshake did not start")
	}
	if err := s.UpdatePowerManagementState("running"); err != nil {
		close(mock.releaseHello)
		t.Fatalf("running notification: %v", err)
	}
	select {
	case <-mock.probeStarted:
		t.Log("baud capability probe started while the host handshake was still blocked")
	case <-time.After(3 * time.Second):
		close(mock.releaseHello)
		s.link.Stop()
		t.Fatal("baud probe did not start during initialization")
	}
	close(mock.releaseHello)
	if err := <-initDone; err != nil {
		t.Fatalf("InitializeNRF52: %v", err)
	}
	s.link.Stop()
}

func TestNRFInitializationInterruptedAfterHostHandshake(t *testing.T) {
	for _, tc := range []struct {
		name      string
		interrupt bool
	}{
		{name: "uninterrupted"},
		{name: "port closed after handshake", interrupt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, mr := newVersionPushService(t)
			mr.HSet("version:mdb", "version", "v1.4.2")
			mock := &initInterruptUSOCK{svc: s, closeAfterHello: tc.interrupt}
			s.usock = mock

			err := s.InitializeNRF52()
			if err != nil {
				t.Fatalf("InitializeNRF52 returned %v", err)
			}
			if tc.interrupt {
				if mock.failedWrites == 0 || mock.streamEnabled || mock.streamSynced {
					t.Fatalf("failed writes=%d, stream enabled=%t, synced=%t", mock.failedWrites, mock.streamEnabled, mock.streamSynced)
				}
				t.Logf("initialization returned success despite %d failed writes and no stream enable or sync", mock.failedWrites)
			} else if mock.failedWrites != 0 || !mock.streamEnabled || !mock.streamSynced {
				t.Fatalf("failed writes=%d, stream enabled=%t, synced=%t", mock.failedWrites, mock.streamEnabled, mock.streamSynced)
			}
		})
	}
}
