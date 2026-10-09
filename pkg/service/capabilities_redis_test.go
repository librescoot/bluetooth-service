package service

import (
	"strings"
	"testing"
)

func TestFileTransferCapabilitiesPreserveLegacyDiscovery(t *testing.T) {
	for _, tc := range []struct {
		tunnel, enabled bool
		want            string
	}{
		{false, false, ""}, {false, true, ""}, {true, false, ":files=1"}, {true, true, ":files=1:data=1"},
	} {
		if got := fileTransferCapabilities(tc.tunnel, tc.enabled); got != tc.want {
			t.Errorf("capabilities(%v,%v)=%q want %q", tc.tunnel, tc.enabled, got, tc.want)
		}
	}
}

func TestPublishCapabilitiesToSystem(t *testing.T) {
	s, server := newVersionPushService(t)
	if err := s.PublishCapabilities(); err != nil {
		t.Fatal(err)
	}
	registry := server.HGet("system", "capabilities")
	if !strings.HasPrefix(registry, "cap:ext:") || !strings.Contains(registry, ":nav=2:") {
		t.Fatalf("system capabilities = %q", registry)
	}
}
