package service

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/librescoot/bluetooth-service/pkg/ble"
	"github.com/librescoot/bluetooth-service/pkg/logger"
)

func TestLinkServiceChangedLogging(t *testing.T) {
	var output bytes.Buffer
	svc := &Service{log: logger.NewLogger(log.New(&output, "", 0), logger.LogLevelInfo)}
	link := newLinkManager(svc)
	key := uint16(ble.TypeLink) + uint16(ble.TypeLinkServiceChanged)

	link.HandleLinkMessage(key, int64(1))
	link.HandleLinkMessage(key, int64(1))
	link.HandleLinkMessage(key, int64(3))
	link.HandleLinkMessage(key, int64(7))
	got := output.String()
	if strings.Count(got, "Bluetooth Service Changed:") != 3 {
		t.Fatalf("expected a log only when flags change, got %q", got)
	}
	for _, want := range []string{
		"requested=true sent=false confirmed=false",
		"requested=true sent=true confirmed=false",
		"requested=true sent=true confirmed=true",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	link.resetToDefault()
	link.HandleLinkMessage(key, int64(7))
	if strings.Count(output.String(), "Bluetooth Service Changed:") != 4 {
		t.Fatal("link restart must log the first status again")
	}
}
