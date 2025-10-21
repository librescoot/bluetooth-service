package service

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"hash/crc32"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/librescoot/bluetooth-service/pkg/logger"
)

const (
	// Default directory for received files
	DefaultFileDir = "/tmp/bluetooth-transfers"

	// Maximum file size allowed (10MB)
	MaxFileSize = 10 * 1024 * 1024

	// Chunk size for file transfers
	// Base64 encoding increases size by 4/3, so 64-byte buffer / (4/3) = 48 bytes max binary
	// 48 bytes - 2 bytes for sequence number = 46 bytes for actual data
	ChunkSize = 46

	// Timeout for a single chunk
	ChunkTimeout = 30 * time.Second

	// Total transfer timeout
	TransferTimeout = 10 * time.Minute
)

// TransferState represents the state of a file transfer
type TransferState int

const (
	StateIdle TransferState = iota
	StateInitialized
	StateTransferring
	StateCompleted
	StateAborted
	StateError
)

// FileTransfer represents an active file transfer
type FileTransfer struct {
	ID             string
	Filename       string
	Size           uint32
	CRC32          uint32
	SHA256         string
	State          TransferState
	Progress       uint32
	Chunks         map[uint16][]byte
	ChunkReceived  map[uint16]bool
	TotalChunks    uint16
	ReceivedChunks uint16
	TempFile       *os.File
	StartTime      time.Time
	LastActivity   time.Time
	mu             sync.RWMutex
	crcHash        hash.Hash32
	sha256Hash     hash.Hash
}

// FileTransferManager manages all file transfers
type FileTransferManager struct {
	transfers    map[string]*FileTransfer
	mu           sync.RWMutex
	log          *logger.Logger
	fileDir      string
	cleanupTimer *time.Timer
}

// NewFileTransferManager creates a new file transfer manager
func NewFileTransferManager(log *logger.Logger) (*FileTransferManager, error) {
	ftm := &FileTransferManager{
		transfers: make(map[string]*FileTransfer),
		log:       log,
		fileDir:   DefaultFileDir,
	}

	// Create the file directory if it doesn't exist
	if err := os.MkdirAll(ftm.fileDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create file directory: %w", err)
	}

	// Start cleanup goroutine
	go ftm.cleanupRoutine()

	return ftm, nil
}

// InitTransfer initializes a new file transfer
func (ftm *FileTransferManager) InitTransfer(filename string, size uint32, crc32Expected uint32) (*FileTransfer, error) {
	ftm.mu.Lock()
	defer ftm.mu.Unlock()

	// Validate file size
	if size > MaxFileSize {
		return nil, fmt.Errorf("file size %d exceeds maximum allowed size %d", size, MaxFileSize)
	}

	// Validate filename
	if filename == "" {
		return nil, fmt.Errorf("filename cannot be empty")
	}

	// Sanitize filename to prevent directory traversal
	filename = filepath.Base(filename)

	// Generate transfer ID
	transferID := fmt.Sprintf("%s_%d", filename, time.Now().UnixNano())

	// Create temp file
	tempFilePath := filepath.Join(ftm.fileDir, fmt.Sprintf(".%s.tmp", transferID))
	tempFile, err := os.Create(tempFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file: %w", err)
	}

	// Calculate total chunks
	totalChunks := (size + ChunkSize - 1) / ChunkSize

	transfer := &FileTransfer{
		ID:            transferID,
		Filename:      filename,
		Size:          size,
		CRC32:         crc32Expected,
		State:         StateInitialized,
		Progress:      0,
		Chunks:        make(map[uint16][]byte),
		ChunkReceived: make(map[uint16]bool),
		TotalChunks:   uint16(totalChunks),
		ReceivedChunks: 0,
		TempFile:      tempFile,
		StartTime:     time.Now(),
		LastActivity:  time.Now(),
		crcHash:       crc32.NewIEEE(),
		sha256Hash:    sha256.New(),
	}

	ftm.transfers[transferID] = transfer
	ftm.log.Infof("Initialized file transfer: ID=%s, Filename=%s, Size=%d, Chunks=%d",
		transferID, filename, size, totalChunks)

	return transfer, nil
}

// HandleChunk processes a received chunk
func (ftm *FileTransferManager) HandleChunk(transferID string, sequence uint16, data []byte) error {
	ftm.mu.RLock()
	transfer, exists := ftm.transfers[transferID]
	ftm.mu.RUnlock()

	if !exists {
		return fmt.Errorf("transfer not found: %s", transferID)
	}

	transfer.mu.Lock()
	defer transfer.mu.Unlock()

	// Check if transfer is in valid state
	if transfer.State != StateInitialized && transfer.State != StateTransferring {
		return fmt.Errorf("transfer not in valid state: %v", transfer.State)
	}

	// Update state if this is the first chunk
	if transfer.State == StateInitialized {
		transfer.State = StateTransferring
	}

	// Check if chunk was already received
	if transfer.ChunkReceived[sequence] {
		ftm.log.Debugf("Chunk %d already received for transfer %s", sequence, transferID)
		return nil
	}

	// Validate sequence number
	if sequence >= transfer.TotalChunks {
		return fmt.Errorf("invalid sequence number %d (max %d)", sequence, transfer.TotalChunks-1)
	}

	// Calculate expected offset
	offset := int64(sequence) * ChunkSize

	// Write chunk to temp file at correct position
	if _, err := transfer.TempFile.WriteAt(data, offset); err != nil {
		return fmt.Errorf("failed to write chunk to file: %w", err)
	}

	// Update hashes
	transfer.crcHash.Write(data)
	transfer.sha256Hash.Write(data)

	// Mark chunk as received
	transfer.ChunkReceived[sequence] = true
	transfer.ReceivedChunks++
	transfer.Progress = uint32(transfer.ReceivedChunks) * 100 / uint32(transfer.TotalChunks)
	transfer.LastActivity = time.Now()

	ftm.log.Debugf("Received chunk %d/%d for transfer %s (progress: %d%%)",
		sequence+1, transfer.TotalChunks, transferID, transfer.Progress)

	// Check if all chunks received
	if transfer.ReceivedChunks == transfer.TotalChunks {
		ftm.log.Infof("All chunks received for transfer %s", transferID)
	}

	return nil
}

// CompleteTransfer finalizes a file transfer
func (ftm *FileTransferManager) CompleteTransfer(transferID string, expectedCRC uint32) error {
	ftm.mu.RLock()
	transfer, exists := ftm.transfers[transferID]
	ftm.mu.RUnlock()

	if !exists {
		return fmt.Errorf("transfer not found: %s", transferID)
	}

	transfer.mu.Lock()
	defer transfer.mu.Unlock()

	// Verify all chunks received
	if transfer.ReceivedChunks != transfer.TotalChunks {
		return fmt.Errorf("not all chunks received: %d/%d", transfer.ReceivedChunks, transfer.TotalChunks)
	}

	// Close temp file
	if err := transfer.TempFile.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %w", err)
	}

	// Verify CRC32
	calculatedCRC := transfer.crcHash.Sum32()
	if calculatedCRC != expectedCRC {
		transfer.State = StateError
		return fmt.Errorf("CRC mismatch: expected %x, got %x", expectedCRC, calculatedCRC)
	}

	// Calculate final SHA256
	transfer.SHA256 = hex.EncodeToString(transfer.sha256Hash.Sum(nil))

	// Move temp file to final location
	tempPath := transfer.TempFile.Name()
	finalPath := filepath.Join(ftm.fileDir, transfer.Filename)

	if err := os.Rename(tempPath, finalPath); err != nil {
		return fmt.Errorf("failed to move file to final location: %w", err)
	}

	transfer.State = StateCompleted
	transfer.LastActivity = time.Now()

	duration := time.Since(transfer.StartTime)
	throughput := float64(transfer.Size) / duration.Seconds() / 1024

	ftm.log.Infof("Transfer completed: ID=%s, File=%s, Size=%d bytes, Duration=%v, Throughput=%.2f KB/s, SHA256=%s",
		transferID, transfer.Filename, transfer.Size, duration, throughput, transfer.SHA256)

	return nil
}

// AbortTransfer cancels an active transfer
func (ftm *FileTransferManager) AbortTransfer(transferID string, reason string) error {
	ftm.mu.RLock()
	transfer, exists := ftm.transfers[transferID]
	ftm.mu.RUnlock()

	if !exists {
		return fmt.Errorf("transfer not found: %s", transferID)
	}

	transfer.mu.Lock()
	defer transfer.mu.Unlock()

	// Close and remove temp file
	if transfer.TempFile != nil {
		transfer.TempFile.Close()
		os.Remove(transfer.TempFile.Name())
	}

	transfer.State = StateAborted
	transfer.LastActivity = time.Now()

	ftm.log.Warnf("Transfer aborted: ID=%s, Reason=%s", transferID, reason)

	return nil
}

// GetTransferStatus returns the current status of a transfer
func (ftm *FileTransferManager) GetTransferStatus(transferID string) (*FileTransfer, error) {
	ftm.mu.RLock()
	defer ftm.mu.RUnlock()

	transfer, exists := ftm.transfers[transferID]
	if !exists {
		return nil, fmt.Errorf("transfer not found: %s", transferID)
	}

	return transfer, nil
}

// cleanupRoutine periodically cleans up old or abandoned transfers
func (ftm *FileTransferManager) cleanupRoutine() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		ftm.mu.Lock()
		now := time.Now()

		for id, transfer := range ftm.transfers {
			transfer.mu.RLock()
			state := transfer.State
			lastActivity := transfer.LastActivity
			tempFile := transfer.TempFile
			transfer.mu.RUnlock()

			// Remove completed or aborted transfers after 5 minutes
			if (state == StateCompleted || state == StateAborted) &&
				now.Sub(lastActivity) > 5*time.Minute {
				delete(ftm.transfers, id)
				ftm.log.Debugf("Removed completed/aborted transfer: %s", id)
				continue
			}

			// Abort transfers that have been inactive for too long
			if (state == StateInitialized || state == StateTransferring) &&
				now.Sub(lastActivity) > TransferTimeout {
				// Close and remove temp file
				if tempFile != nil {
					tempFile.Close()
					os.Remove(tempFile.Name())
				}

				transfer.mu.Lock()
				transfer.State = StateError
				transfer.mu.Unlock()

				ftm.log.Warnf("Transfer timed out and was aborted: %s", id)
			}

			// Remove error state transfers after 5 minutes
			if state == StateError && now.Sub(lastActivity) > 5*time.Minute {
				delete(ftm.transfers, id)
				ftm.log.Debugf("Removed timed-out transfer: %s", id)
				continue
			}
		}

		ftm.mu.Unlock()
	}
}

