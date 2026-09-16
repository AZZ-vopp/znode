package relay

import (
	"context"
	"net"
	"testing"
	"time"

	proxyproto "github.com/pires/go-proxyproto"
)

func TestSupervisorReplacesSameListenConfig(t *testing.T) {
	backendA, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer backendA.Close()
	backendB, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer backendB.Close()
	probe, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	listen := probe.LocalAddr().String()
	_ = probe.Close()
	s := NewSupervisor(context.Background())
	defer s.Close()
	first := Config{ID: "node-1", Network: "udp4", Listen: listen, Upstream: backendA.LocalAddr().String(), TTL: time.Minute, MaxFlows: 8}
	if err := s.Reconcile([]Config{first}); err != nil {
		t.Fatal(err)
	}
	second := first
	second.Upstream = backendB.LocalAddr().String()
	if err := s.Reconcile([]Config{second}); err != nil {
		t.Fatalf("same-listen replacement: %v", err)
	}
}

func TestDatagramUsesProxyV2UDP(t *testing.T) {
	client := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12000}
	upstream := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 443}
	packet, err := datagram(client, upstream, []byte("quic"))
	if err != nil {
		t.Fatal(err)
	}
	header, payload, err := proxyproto.ParseUDPDatagram(packet)
	if err != nil {
		t.Fatal(err)
	}
	if header.Version != 2 || header.TransportProtocol != proxyproto.UDPv4 || string(payload) != "quic" {
		t.Fatalf("unexpected proxy datagram")
	}
}

func TestRelayForwardsAndReturnsReply(t *testing.T) {
	backend, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &instance{cfg: Config{ID: "test", Network: "udp4", TTL: time.Second, MaxFlows: 2}, conn: listener, upstream: backend.LocalAddr().(*net.UDPAddr), flows: map[string]*flow{}, cancel: cancel, done: make(chan error, 1)}
	go r.serve(ctx)
	defer r.close()
	client, err := net.DialUDP("udp4", nil, listener.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err = client.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 65535)
	_ = backend.SetReadDeadline(time.Now().Add(time.Second))
	n, peer, err := backend.ReadFromUDP(b)
	if err != nil {
		t.Fatal(err)
	}
	_, payload, err := proxyproto.ParseUDPDatagram(b[:n])
	if err != nil || string(payload) != "hello" {
		t.Fatalf("invalid forwarded packet: %v %q", err, payload)
	}
	_, _ = backend.WriteToUDP([]byte("world"), peer)
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	n, err = client.Read(b)
	if err != nil || string(b[:n]) != "world" {
		t.Fatalf("reply=%q err=%v", b[:n], err)
	}
}

func TestRelayFlowLimitExpires(t *testing.T) {
	backend, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	r := &instance{cfg: Config{ID: "test", Network: "udp4", TTL: time.Millisecond, MaxFlows: 1}, upstream: backend.LocalAddr().(*net.UDPAddr), flows: map[string]*flow{}}
	first := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 10001}
	if _, err := r.forClient(first); err != nil {
		t.Fatal(err)
	}
	if _, err := r.forClient(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 10002}); err == nil {
		t.Fatal("expected flow limit")
	}
	r.mu.Lock()
	r.flows[first.String()].seen = time.Now().Add(-time.Second)
	r.mu.Unlock()
	if _, err := r.forClient(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 10002}); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	for _, f := range r.flows {
		_ = f.upstream.Close()
	}
	r.mu.Unlock()
}
