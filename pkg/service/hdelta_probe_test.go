package service

import (
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func TestProbeHdelta(t *testing.T) {
	for _, tc := range []struct {
		component, value string
		want             bool
	}{
		{"mdb", "1", true}, {"dbc", "0", false}, {"dbc", "unexpected", false},
	} {
		t.Run(tc.component+tc.value, func(t *testing.T) {
			mr := miniredis.RunT(t)
			s := newDestinationService(t, mr)
			s.ipc = s.destIPC
			done := make(chan bool, 1)
			go func() { done <- probeHdelta(s.ipc, tc.component) }()
			var command string
			for i := 0; i < 100; i++ {
				items, _ := mr.List("scooter:update:" + tc.component)
				if len(items) > 0 {
					command = items[0]
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			if !strings.HasPrefix(command, "probe-hdelta:") {
				t.Fatalf("command %q", command)
			}
			nonce := strings.TrimPrefix(command, "probe-hdelta:")
			if len(nonce) != 32 {
				t.Fatalf("nonce %q", nonce)
			}
			mr.Set("ota:hdelta-probe:"+tc.component+":"+nonce, tc.value)
			if got := <-done; got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
	if probeHdelta(nil, "mdb") {
		t.Fatal("nil client accepted")
	}
}
