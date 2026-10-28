package service

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/librescoot/bluetooth-service/pkg/ble"
)

const (
	hostSessionProtocolVersion int32 = 1
	hostCapExternalTemperature int32 = 1 << 0
	hostCapabilitiesRequested        = hostCapExternalTemperature
	hostSessionAckTimeout            = 300 * time.Millisecond
)

type hostSessionAck struct {
	version      int32
	capabilities int32
	nonce        int32
}

func newHostSessionNonce() int32 {
	var value [4]byte
	if _, err := rand.Read(value[:]); err == nil {
		nonce := int32(binary.LittleEndian.Uint32(value[:]) & 0x7fffffff)
		if nonce != 0 {
			return nonce
		}
	}

	return int32(uint64(time.Now().UnixNano())%0x7ffffffe) + 1
}

func writeHostSessionRequest(sock usockWriter, nonce int32) error {
	if sock == nil {
		return fmt.Errorf("USOCK connection is not initialized")
	}

	messageType := ble.TypeBLEVersion
	message := map[uint16]map[uint16]interface{}{
		uint16(messageType): {
			uint16(messageType) + uint16(ble.TypeBLEVersionRequest): uint16(0),
			uint16(messageType) + uint16(ble.TypeBLEVersionHostSession): []int32{
				hostSessionProtocolVersion,
				hostCapabilitiesRequested,
				nonce,
			},
		},
	}

	data, err := cbor.Marshal(message)
	if err != nil {
		return fmt.Errorf("marshal host session request: %w", err)
	}
	return sock.WriteWithFrameID(byte(messageType&0xff), data)
}

func (s *Service) hostSessionChannel() chan hostSessionAck {
	s.hostSessionMu.Lock()
	defer s.hostSessionMu.Unlock()
	if s.hostSessionAckCh == nil {
		s.hostSessionAckCh = make(chan hostSessionAck, 4)
	}
	return s.hostSessionAckCh
}

func (s *Service) setHostCapabilities(capabilities int32) {
	s.hostSessionMu.Lock()
	s.hostCapabilities = capabilities
	s.hostSessionMu.Unlock()
}

func drainHostSessionAcks(ackCh chan hostSessionAck) {
	for {
		select {
		case <-ackCh:
		default:
			return
		}
	}
}

func (s *Service) negotiateHostSession() error {
	s.hostSessionInitMu.Lock()
	defer s.hostSessionInitMu.Unlock()

	ackCh := s.hostSessionChannel()
	drainHostSessionAcks(ackCh)
	s.setHostCapabilities(0)
	nonce := newHostSessionNonce()
	if err := writeHostSessionRequest(s.GetUSock(), nonce); err != nil {
		return err
	}

	timer := time.NewTimer(hostSessionAckTimeout)
	defer timer.Stop()
	for {
		select {
		case ack := <-ackCh:
			if ack.version != hostSessionProtocolVersion || ack.nonce != nonce {
				continue
			}
			if ack.capabilities < 0 || ack.capabilities&^hostCapabilitiesRequested != 0 {
				continue
			}
			s.setHostCapabilities(ack.capabilities)
			s.log.Infof("nRF host capabilities negotiated: 0x%x", ack.capabilities)
			return nil
		case <-timer.C:
			s.log.Infof("nRF host capability handshake unavailable; using legacy mode")
			return nil
		}
	}
}

func (s *Service) handleHostSessionAck(value interface{}) {
	values, ok := value.([]interface{})
	if !ok || len(values) != 3 {
		s.log.Warnf("Invalid nRF host-session acknowledgment: %v", value)
		return
	}

	decoded := [3]int32{}
	for i, value := range values {
		v, ok := convertToInt(value)
		if !ok || v < 0 || int64(v) > int64(^uint32(0)>>1) {
			s.log.Warnf("Invalid nRF host-session acknowledgment: %v", values)
			return
		}
		decoded[i] = int32(v)
	}

	ack := hostSessionAck{
		version:      decoded[0],
		capabilities: decoded[1],
		nonce:        decoded[2],
	}
	select {
	case s.hostSessionChannel() <- ack:
	default:
	}
}
