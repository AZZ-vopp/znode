package cmd

import (
	"context"
	"testing"
	"time"
)

func TestRunUDPRelayRejectsInvalidConfig(t *testing.T) {
	err := runUDPRelay(context.Background(), udpRelayConfig{Listen: "127.0.0.1:0", Upstream: "127.0.0.1:1", TTL: 0, MaxFlows: 1})
	if err == nil || err.Error() != "udp relay ttl must be greater than zero" {
		t.Fatalf("unexpected error: %v", err)
	}
}
func TestRunUDPRelayRejectsAmbiguousNetwork(t *testing.T) {
	err := runUDPRelay(context.Background(), udpRelayConfig{Network: "udp", Listen: "127.0.0.1:0", Upstream: "127.0.0.1:1", TTL: time.Second, MaxFlows: 1})
	if err == nil || err.Error() != "udp relay network must be udp4 or udp6" {
		t.Fatalf("unexpected error: %v", err)
	}
}
