package service

import (
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/librescoot/bluetooth-service/pkg/ble"
	"github.com/librescoot/bluetooth-service/pkg/logger"
	"github.com/librescoot/bluetooth-service/pkg/usock"
)

type hostSessionTestUSOCK struct {
	svc  *Service
	data []byte
}

func (m *hostSessionTestUSOCK) WriteWithFrameID(_ byte, data []byte) error {
	m.data = append([]byte(nil), data...)

	var message map[uint16]map[uint16]cbor.RawMessage
	if err := cbor.Unmarshal(data, &message); err != nil {
		return err
	}
	var hello []int32
	key := uint16(ble.TypeBLEVersion) + uint16(ble.TypeBLEVersionHostSession)
	if err := cbor.Unmarshal(message[uint16(ble.TypeBLEVersion)][key], &hello); err != nil {
		return err
	}
	m.svc.handleHostSessionAck([]interface{}{
		uint64(hello[0]),
		uint64(hostCapExternalTemperature),
		uint64(hello[2]),
	})
	return nil
}

func (m *hostSessionTestUSOCK) Close() error { return nil }

func TestHostSessionRequestCodec(t *testing.T) {
	mock := &mockUSOCK{}
	const nonce int32 = 12345678
	if err := writeHostSessionRequest(mock, nonce); err != nil {
		t.Fatal(err)
	}

	if got := mock.messageCount(); got != 1 {
		t.Fatalf("messages = %d, want 1", got)
	}
	message := mock.lastMessage()
	if message.frameID != byte(ble.TypeBLEVersion&0xff) {
		t.Fatalf("frame ID = %#x, want %#x", message.frameID, byte(ble.TypeBLEVersion&0xff))
	}

	var decoded map[uint16]map[uint16]cbor.RawMessage
	if err := cbor.Unmarshal(message.data, &decoded); err != nil {
		t.Fatal(err)
	}
	params := decoded[uint16(ble.TypeBLEVersion)]

	requestKey := uint16(ble.TypeBLEVersion) + uint16(ble.TypeBLEVersionRequest)
	var request uint16
	if err := cbor.Unmarshal(params[requestKey], &request); err != nil {
		t.Fatalf("decode version request: %v", err)
	}
	if request != 0 {
		t.Fatalf("version request = %d, want 0", request)
	}

	hostSessionKey := uint16(ble.TypeBLEVersion) + uint16(ble.TypeBLEVersionHostSession)
	var hello []int32
	if err := cbor.Unmarshal(params[hostSessionKey], &hello); err != nil {
		t.Fatalf("decode host session: %v", err)
	}
	if len(hello) != 3 || hello[0] != hostSessionProtocolVersion ||
		hello[1] != hostCapabilitiesRequested || hello[2] != nonce {
		t.Fatalf("host session = %v", hello)
	}
}

func TestHostSessionNegotiationAcceptsMatchingAck(t *testing.T) {
	s := &Service{
		log:              logger.NewLogger(nil, logger.LogLevelNone),
		hostSessionAckCh: make(chan hostSessionAck, 4),
	}
	mock := &hostSessionTestUSOCK{svc: s}
	s.usock = mock

	if err := s.negotiateHostSession(); err != nil {
		t.Fatal(err)
	}

	s.hostSessionMu.Lock()
	capabilities := s.hostCapabilities
	s.hostSessionMu.Unlock()
	if capabilities != hostCapExternalTemperature {
		t.Fatalf("negotiated capabilities = %#x, want %#x", capabilities, hostCapExternalTemperature)
	}
}

func TestHostSessionAckDispatch(t *testing.T) {
	s := &Service{log: logger.NewLogger(nil, logger.LogLevelNone)}
	key := uint16(ble.TypeBLEVersion) + uint16(ble.TypeBLEVersionHostSession)
	data, err := cbor.Marshal(map[uint16]map[uint16][]int32{
		uint16(ble.TypeBLEVersion): {
			key: {hostSessionProtocolVersion, hostCapExternalTemperature, 42},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	frameID := byte(ble.TypeBLEVersion & 0xff)
	s.HandleUSockMessage(frameID, &usock.Payload{
		ID:   frameID,
		Data: data,
		Size: len(data),
	})

	select {
	case ack := <-s.hostSessionChannel():
		if ack.version != hostSessionProtocolVersion ||
			ack.capabilities != hostCapExternalTemperature || ack.nonce != 42 {
			t.Fatalf("unexpected acknowledgment: %+v", ack)
		}
	default:
		t.Fatal("host-session acknowledgment was not dispatched")
	}
}

func TestHostSessionAckRejectsMalformedValue(t *testing.T) {
	s := &Service{log: logger.NewLogger(nil, logger.LogLevelNone)}
	s.handleHostSessionAck([]interface{}{uint64(1), "invalid", uint64(2)})

	select {
	case ack := <-s.hostSessionChannel():
		t.Fatalf("unexpected acknowledgment: %+v", ack)
	default:
	}
}
