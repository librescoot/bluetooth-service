package service

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/librescoot/bluetooth-service/pkg/logger"
	"github.com/librescoot/bluetooth-service/pkg/usock"
)

type uartCloseHook struct {
	mockUSOCK
	onClose func()
}

func (m *uartCloseHook) Close() error {
	m.onClose()
	return nil
}

func TestBaudReopenUnpublishesClosingUART(t *testing.T) {
	s := &Service{log: logger.NewLogger(nil, logger.LogLevelNone)}
	s.SetSerialConfig(filepath.Join(t.TempDir(), "missing-uart"), linkBaudDefault, func(*usock.Payload) {}, nil)
	old := &uartCloseHook{onClose: func() {
		if err := s.UpdateVehicleState("parked"); !errors.Is(err, errLinkUnavailable) {
			t.Errorf("update during UART close: got %v, want errLinkUnavailable", err)
		}
	}}
	s.usock = old

	if err := s.reopenUSockAtBaud(linkBaudFast); err == nil {
		t.Fatal("opening the missing UART succeeded")
	}
	if old.messageCount() != 0 {
		t.Fatal("update wrote to the closing UART")
	}
	if err := s.UpdateVehicleState("stand-by"); !errors.Is(err, errLinkUnavailable) {
		t.Fatalf("update after failed reopen: got %v, want errLinkUnavailable", err)
	}
}

func TestFailedMileageWriteDoesNotSuppressNextUpdate(t *testing.T) {
	port := &mockUSOCK{writeError: os.ErrClosed}
	s := &Service{usock: port, log: logger.NewLogger(nil, logger.LogLevelNone)}
	if err := s.UpdateMileage("1234"); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("update on closed UART: got %v, want os.ErrClosed", err)
	}
	port.writeError = nil
	for i := 0; i < 2; i++ {
		if err := s.UpdateMileage("1234"); err != nil {
			t.Fatal(err)
		}
	}
	if got := port.messageCount(); got != 1 {
		t.Fatalf("wrote %d mileage updates after recovery, want 1", got)
	}
}
