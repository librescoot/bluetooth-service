package service

import (
	"strings"
	"testing"
)

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
