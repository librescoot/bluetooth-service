package usock

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/tarm/serial"
)

const (
	MaxPayloadLength = 1024
	SyncByte1       = 0xF6
	SyncByte2       = 0xD9
)

// State machine states
const (
	StateSync1 = iota
	StateSync2
	StateFrameID
	StatePayloadLen1
	StatePayloadLen2
	StateHeaderCRC1
	StateHeaderCRC2
	StatePayload
	StatePayloadCRC1
	StatePayloadCRC2
)

// State represents the state of the USOCK state machine
type State int

// Frame represents a USOCK frame
type Frame struct {
	ID         byte
	PayloadLen uint16
	HeaderCRC  uint16
	Payload    []byte
	PayloadCRC uint16
}

// Payload represents a received message payload
type Payload struct {
	ID   byte   // Frame ID
	Data []byte // Payload data
	Size int    // Size of the payload
}

// USOCK represents a UART socket connection to the nRF52
type USOCK struct {
	port     *serial.Port
	handler  func(*Payload)
	stopChan chan struct{}
	wg       sync.WaitGroup
	state    State
	frame    Frame
	buffer   []byte
	mu       sync.Mutex
}

// CRC-16/ARC lookup table
var crc16Table = []uint16{
	0x0000, 0xC0C1, 0xC181, 0x0140, 0xC301, 0x03C0, 0x0280, 0xC241, 0xC601, 0x06C0, 0x0780, 0xC741,
	0x0500, 0xC5C1, 0xC481, 0x0440, 0xCC01, 0x0CC0, 0x0D80, 0xCD41, 0x0F00, 0xCFC1, 0xCE81, 0x0E40,
	0x0A00, 0xCAC1, 0xCB81, 0x0B40, 0xC901, 0x09C0, 0x0880, 0xC841, 0xD801, 0x18C0, 0x1980, 0xD941,
	0x1B00, 0xDBC1, 0xDA81, 0x1A40, 0x1E00, 0xDEC1, 0xDF81, 0x1F40, 0xDD01, 0x1DC0, 0x1C80, 0xDC41,
	0x1400, 0xD4C1, 0xD581, 0x1540, 0xD701, 0x17C0, 0x1680, 0xD641, 0xD201, 0x12C0, 0x1380, 0xD341,
	0x1100, 0xD1C1, 0xD081, 0x1040, 0xF001, 0x30C0, 0x3180, 0xF141, 0x3300, 0xF3C1, 0xF281, 0x3240,
	0x3600, 0xF6C1, 0xF781, 0x3740, 0xF501, 0x35C0, 0x3480, 0xF441, 0x3C00, 0xFCC1, 0xFD81, 0x3D40,
	0xFF01, 0x3FC0, 0x3E80, 0xFE41, 0xFA01, 0x3AC0, 0x3B80, 0xFB41, 0x3900, 0xF9C1, 0xF881, 0x3840,
	0x2800, 0xE8C1, 0xE981, 0x2940, 0xEB01, 0x2BC0, 0x2A80, 0xEA41, 0xEE01, 0x2EC0, 0x2F80, 0xEF41,
	0x2D00, 0xEDC1, 0xEC81, 0x2C40, 0xE401, 0x24C0, 0x2580, 0xE541, 0x2700, 0xE7C1, 0xE681, 0x2640,
	0x2200, 0xE2C1, 0xE381, 0x2340, 0xE101, 0x21C0, 0x2080, 0xE041, 0xA001, 0x60C0, 0x6180, 0xA141,
	0x6300, 0xA3C1, 0xA281, 0x6240, 0x6600, 0xA6C1, 0xA781, 0x6740, 0xA501, 0x65C0, 0x6480, 0xA441,
	0x6C00, 0xACC1, 0xAD81, 0x6D40, 0xAF01, 0x6FC0, 0x6E80, 0xAE41, 0xAA01, 0x6AC0, 0x6B80, 0xAB41,
	0x6900, 0xA9C1, 0xA881, 0x6840, 0x7800, 0xB8C1, 0xB981, 0x7940, 0xBB01, 0x7BC0, 0x7A80, 0xBA41,
	0xBE01, 0x7EC0, 0x7F80, 0xBF41, 0x7D00, 0xBDC1, 0xBC81, 0x7C40, 0xB401, 0x74C0, 0x7580, 0xB541,
	0x7700, 0xB7C1, 0xB681, 0x7640, 0x7200, 0xB2C1, 0xB381, 0x7340, 0xB101, 0x71C0, 0x7080, 0xB041,
	0x5000, 0x90C1, 0x9181, 0x5140, 0x9301, 0x53C0, 0x5280, 0x9241, 0x9601, 0x56C0, 0x5780, 0x9741,
	0x5500, 0x95C1, 0x9481, 0x5440, 0x9C01, 0x5CC0, 0x5D80, 0x9D41, 0x5F00, 0x9FC1, 0x9E81, 0x5E40,
	0x5A00, 0x9AC1, 0x9B81, 0x5B40, 0x9901, 0x59C0, 0x5880, 0x9841, 0x8801, 0x48C0, 0x4980, 0x8941,
	0x4B00, 0x8BC1, 0x8A81, 0x4A40, 0x4E00, 0x8EC1, 0x8F81, 0x4F40, 0x8D01, 0x4DC0, 0x4C80, 0x8C41,
	0x4400, 0x84C1, 0x8581, 0x4540, 0x8701, 0x47C0, 0x4680, 0x8641, 0x8201, 0x42C0, 0x4380, 0x8341,
	0x4100, 0x81C1, 0x8081, 0x4040,
}

// New creates a new USOCK connection
func New(devicePath string, baudRate int, handler func(*Payload)) (*USOCK, error) {
	// First clear UART attributes to ensure a clean start
	if err := clearUARTAttributes(devicePath); err != nil {
		return nil, fmt.Errorf("failed to clear UART attributes: %v", err)
	}

	config := &serial.Config{
		Name:        devicePath,
		Baud:        baudRate,
		Size:        8,
		Parity:      serial.ParityNone,
		StopBits:    serial.Stop1,
		ReadTimeout: 0,
	}

	// Open the port
	port, err := serial.OpenPort(config)
	if err != nil {
		return nil, fmt.Errorf("failed to open serial port: %v", err)
	}

	// Create USOCK instance
	usock := &USOCK{
		port:     port,
		handler:  handler,
		stopChan: make(chan struct{}),
		state:    StateSync1,
		buffer:   make([]byte, 0, 256),
	}

	// Start read loop
	usock.wg.Add(1)
	go usock.readLoop()

	return usock, nil
}

// clearUARTAttributes clears the UART attributes to ensure a clean start
func clearUARTAttributes(devicePath string) error {
	// With tarm/serial, we can't directly manipulate the terminal attributes
	// Instead, we'll open the port with default settings and then close it
	config := &serial.Config{
		Name:        devicePath,
		Baud:        9600,
		Size:        8,
		Parity:      serial.ParityNone,
		StopBits:    serial.Stop1,
		ReadTimeout: 0,
	}
	
	port, err := serial.OpenPort(config)
	if err != nil {
		return fmt.Errorf("failed to open serial port for attribute clearing: %v", err)
	}
	
	// Close the port to release resources
	err = port.Close()
	if err != nil {
		return fmt.Errorf("failed to close serial port after attribute clearing: %v", err)
	}
	
	// Wait a moment for the port to fully close
	time.Sleep(100 * time.Millisecond)
	
	return nil
}

// WriteWithFrameID sends data to the nRF52 with a specific frame ID
func (u *USOCK) WriteWithFrameID(frameID byte, data []byte) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	if len(data) > MaxPayloadLength {
		return fmt.Errorf("payload size exceeds maximum length of %d bytes", MaxPayloadLength)
	}

	// Create a frame with the specified ID
	frame := Frame{
		ID:         frameID,
		PayloadLen: uint16(len(data)),
		Payload:    data,
	}

	// Construct the header first (sync bytes + frame ID + payload length)
	header := []byte{SyncByte1, SyncByte2, frame.ID}
	lenBytes := make([]byte, 2)
	binary.LittleEndian.PutUint16(lenBytes, frame.PayloadLen)
	header = append(header, lenBytes...)

	// Calculate header CRC over the entire header at once
	frame.HeaderCRC = calculateCRC16(header, 0)

	// Calculate payload CRC over the entire payload at once
	frame.PayloadCRC = calculateCRC16(frame.Payload, 0)

	// Log detailed frame information
	log.Printf("TX Frame: ID=0x%02x, Len=%d, HeaderCRC=0x%04x, PayloadCRC=0x%04x", 
		frame.ID, frame.PayloadLen, frame.HeaderCRC, frame.PayloadCRC)
	
	// Log the payload in hex format for debugging
	log.Printf("TX Payload: %s", hex.EncodeToString(frame.Payload))

	// Construct the complete frame in a single buffer to send all at once
	completeFrame := make([]byte, 0, 7+len(frame.Payload)+2) // 7 bytes header + payload + 2 bytes CRC
	
	// Add header (sync bytes + frame ID + payload length)
	completeFrame = append(completeFrame, header...)
	
	// Add header CRC (little-endian)
	completeFrame = append(completeFrame, byte(frame.HeaderCRC&0xFF), byte((frame.HeaderCRC>>8)&0xFF))
	
	// Add payload
	completeFrame = append(completeFrame, frame.Payload...)
	
	// Add payload CRC (little-endian)
	completeFrame = append(completeFrame, byte(frame.PayloadCRC&0xFF), byte((frame.PayloadCRC>>8)&0xFF))
	
	// Log the complete frame in hex format for debugging
	log.Printf("TX Complete Frame: %s", hex.EncodeToString(completeFrame))
	
	// Write the complete frame in a single operation
	if _, err := u.port.Write(completeFrame); err != nil {
		return fmt.Errorf("failed to write frame: %v", err)
	}

	return nil
}

// Write sends data to the nRF52
// This is kept for backward compatibility but now uses WriteWithFrameID internally
func (u *USOCK) Write(data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("cannot write empty data")
	}
	
	// Use the first byte as the frame ID and the rest as payload
	frameID := data[0]
	payload := data[1:]
	
	return u.WriteWithFrameID(frameID, payload)
}

// Close closes the USOCK connection
func (u *USOCK) Close() error {
	close(u.stopChan)
	u.wg.Wait()
	return u.port.Close()
}

// readLoop continuously reads from the serial port
func (u *USOCK) readLoop() {
	defer u.wg.Done()

	buf := make([]byte, 1) // Read one byte at a time for more precise control
	log.Printf("Starting serial read loop")

	for {
		select {
		case <-u.stopChan:
			return
		default:
			// Use blocking read with no timeout
			n, err := u.port.Read(buf)
			if err != nil {
				if err != io.EOF {
					log.Printf("Error reading from serial port: %v", err)
					time.Sleep(10 * time.Millisecond)
				}
				continue
			}

			if n == 0 {
				continue
			}

			// Process the received byte
			b := buf[0]
			u.processByte(b)
		}
	}
}

// processByte processes a single byte through the state machine
func (u *USOCK) processByte(b byte) {
	// Process the byte based on the current state
	switch u.state {
	case StateSync1:
		if b == SyncByte1 {
			u.state = StateSync2
			u.buffer = u.buffer[:0] // Clear buffer
			u.buffer = append(u.buffer, b)
		}
	case StateSync2:
		if b == SyncByte2 {
			u.state = StateFrameID
			u.buffer = append(u.buffer, b)
		} else {
			u.state = StateSync1
		}
	case StateFrameID:
		u.frame.ID = b
		u.buffer = append(u.buffer, b)
		u.state = StatePayloadLen1
	case StatePayloadLen1:
		u.frame.PayloadLen = uint16(b)
		u.buffer = append(u.buffer, b)
		u.state = StatePayloadLen2
	case StatePayloadLen2:
		u.frame.PayloadLen |= uint16(b) << 8
		u.buffer = append(u.buffer, b)
		u.state = StateHeaderCRC1
		
		// Calculate header CRC over the entire header at once
		u.frame.HeaderCRC = calculateCRC16(u.buffer, 0)
		
		// Validate payload length
		if u.frame.PayloadLen > MaxPayloadLength {
			log.Printf("RX Error: Invalid payload length: %d (max: %d)", 
				u.frame.PayloadLen, MaxPayloadLength)
			u.state = StateSync1
		}
	case StateHeaderCRC1:
		u.frame.HeaderCRC = uint16(b)
		u.state = StateHeaderCRC2
	case StateHeaderCRC2:
		u.frame.HeaderCRC |= uint16(b) << 8
		
		// Calculate CRC for the header (sync bytes + frame ID + payload length)
		calculatedCRC := calculateCRC16(u.buffer, 0)
		
		// Validate header CRC
		if calculatedCRC != u.frame.HeaderCRC {
			log.Printf("RX Error: Invalid header CRC: calculated=0x%04x, received=0x%04x", 
				calculatedCRC, u.frame.HeaderCRC)
			u.state = StateSync1
			return
		}
		
		// Prepare for payload
		u.frame.Payload = make([]byte, 0, u.frame.PayloadLen)
		u.buffer = u.buffer[:0] // Clear buffer for payload CRC calculation
		u.state = StatePayload
	case StatePayload:
		u.frame.Payload = append(u.frame.Payload, b)
		u.buffer = append(u.buffer, b)
		
		// Check if we've received the entire payload
		if uint16(len(u.frame.Payload)) >= u.frame.PayloadLen {
			u.state = StatePayloadCRC1
			// Calculate payload CRC over the entire payload at once
			u.frame.PayloadCRC = calculateCRC16(u.buffer, 0)
		}
	case StatePayloadCRC1:
		u.frame.PayloadCRC = uint16(b)
		u.state = StatePayloadCRC2
	case StatePayloadCRC2:
		u.frame.PayloadCRC |= uint16(b) << 8
		
		// Calculate CRC for the payload
		calculatedCRC := calculateCRC16(u.buffer, 0)
		
		// Validate payload CRC
		if calculatedCRC != u.frame.PayloadCRC {
			log.Printf("RX Error: Invalid payload CRC: calculated=0x%04x, received=0x%04x", 
				calculatedCRC, u.frame.PayloadCRC)
			u.state = StateSync1
			return
		}
		
		// Log successful frame reception with detailed information
		log.Printf("RX Frame: ID=0x%02x, Len=%d, HeaderCRC=0x%04x, PayloadCRC=0x%04x", 
			u.frame.ID, u.frame.PayloadLen, u.frame.HeaderCRC, u.frame.PayloadCRC)
		log.Printf("RX Payload: %s", hex.EncodeToString(u.frame.Payload))
		
		// Create a copy of the payload to avoid data races
		payload := make([]byte, len(u.frame.Payload))
		copy(payload, u.frame.Payload)
		
		// Call the callback with the payload
		if u.handler != nil {
			go u.handler(&Payload{
				ID:   u.frame.ID,
				Data: payload,
				Size: len(payload),
			})
		}
		
		// Reset state machine
		u.state = StateSync1
	}
}

// calculateCRC16 calculates the CRC16 checksum for the given data
func calculateCRC16(data []byte, seed uint16) uint16 {
	crc := seed
	for _, b := range data {
		idx := uint16(crc^uint16(b)) & 0xff
		crc = (crc >> 8) ^ crc16Table[idx]
	}
	return crc
}

// calculateCRC8 calculates an 8-bit CRC by XORing all bytes
func calculateCRC8(data []byte) uint8 {
	var crc uint8
	for _, b := range data {
		crc ^= b
	}
	return crc
} 