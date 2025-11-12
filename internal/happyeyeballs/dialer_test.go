package happyeyeballs

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type stubResolver struct {
	ips []net.IPAddr
}

func (s stubResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return append([]net.IPAddr(nil), s.ips...), nil
}

func TestDialContextPrefersIPv6(t *testing.T) {
	resolver := stubResolver{ips: []net.IPAddr{
		{IP: net.ParseIP("2001:db8::1")},
		{IP: net.ParseIP("2001:db8::2")},
		{IP: net.ParseIP("192.0.2.10")},
	}}

	var mu sync.Mutex
	var calls []string

    dialer := &Dialer{
        Resolver:      resolver,
        FallbackDelay: 10 * time.Millisecond,
        Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			mu.Lock()
			calls = append(calls, address)
			mu.Unlock()
			return nil, errors.New("dial failed")
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := dialer.DialContext(ctx, "tcp", "example.com:80")
	if err == nil {
		t.Fatal("expected error")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) == 0 {
		t.Fatalf("expected at least one dial attempt, got %d", len(calls))
	}
	if calls[0] != net.JoinHostPort("2001:db8::1", "80") {
		t.Fatalf("expected first dial to use IPv6, got %s", calls[0])
	}
}

func TestDialContextSuccessfulFallback(t *testing.T) {
	resolver := stubResolver{ips: []net.IPAddr{
		{IP: net.ParseIP("2001:db8::1")},
		{IP: net.ParseIP("192.0.2.10")},
	}}

	successConn, peer := net.Pipe()
	defer peer.Close()

    dialer := &Dialer{
        Resolver:      resolver,
        FallbackDelay: 5 * time.Millisecond,
        Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			if address == net.JoinHostPort("192.0.2.10", "80") {
				return successConn, nil
			}
			time.Sleep(30 * time.Millisecond)
			return nil, errors.New("ipv6 unreachable")
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	conn, err := dialer.DialContext(ctx, "tcp", "example.com:80")
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if conn != successConn {
		t.Fatalf("returned unexpected connection")
	}
	_ = conn.Close()
}

func TestDialContextSequentialWhenDisabled(t *testing.T) {
	resolver := stubResolver{ips: []net.IPAddr{
		{IP: net.ParseIP("2001:db8::1")},
		{IP: net.ParseIP("192.0.2.10")},
	}}

	var active int32
	calls := make([]string, 0, 2)
	var mu sync.Mutex

	successConn, peer := net.Pipe()
	defer peer.Close()

    dialer := &Dialer{
        Resolver:      resolver,
        FallbackDelay: -1,
        Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			if n := atomic.AddInt32(&active, 1); n > 1 {
				t.Fatalf("dial attempts ran concurrently: %d", n)
			}
			defer atomic.AddInt32(&active, -1)

			mu.Lock()
			calls = append(calls, address)
			mu.Unlock()

			if address == net.JoinHostPort("192.0.2.10", "80") {
				return successConn, nil
			}
			return nil, errors.New("ipv6 unreachable")
		},
	}

	conn, err := dialer.DialContext(context.Background(), "tcp", "example.com:80")
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if conn != successConn {
		t.Fatalf("unexpected connection returned")
	}
	_ = conn.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("expected two sequential attempts, got %d", len(calls))
	}
}
