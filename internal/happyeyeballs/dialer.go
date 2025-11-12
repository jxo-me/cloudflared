package happyeyeballs

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"
)

// DefaultFallbackDelay is the delay between connection attempts for different address families.
const DefaultFallbackDelay = 250 * time.Millisecond

// IPResolver represents a DNS resolver capable of returning IP addresses for a host.
type IPResolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// DialFunc dials the provided network and address within the supplied context.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Dialer implements the Happy Eyeballs (RFC 8305) algorithm on top of an underlying dialer.
type Dialer struct {
	// Base provides the actual network dialing implementation. When nil, a zero net.Dialer is used.
	Base *net.Dialer

	// Resolver is used to resolve hostnames into IP addresses. When nil the net.DefaultResolver is used.
	Resolver IPResolver

	// FallbackDelay controls the delay between connection attempts for different address families. A negative
	// value disables Happy Eyeballs and falls back to sequential dialing. A zero value uses DefaultFallbackDelay.
	FallbackDelay time.Duration

	// PreferIPv6 indicates whether IPv6 addresses should be attempted before IPv4. When nil, IPv6 is preferred.
	PreferIPv6 *bool

	// Dial overrides the dialing behaviour for testing. When nil, Base.DialContext is used.
	Dial DialFunc
}

// DialContext attempts to establish a connection to the address using the Happy Eyeballs algorithm.
func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}

	if ip := net.ParseIP(host); ip != nil {
		return d.dial(ctx, network, address)
	}

	resolver := d.resolver()
	ips, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, &net.DNSError{Err: "no such host", Name: host}
	}

	var v6, v4 []net.IPAddr
	for _, ipAddr := range ips {
		if ipAddr.IP.To4() != nil {
			v4 = append(v4, ipAddr)
		} else {
			v6 = append(v6, ipAddr)
		}
	}

	preferIPv6 := true
	if d != nil && d.PreferIPv6 != nil {
		preferIPv6 = *d.PreferIPv6
	}

	candidates := interleaveAddresses(v6, v4, preferIPv6)
	if len(candidates) == 0 {
		return nil, &net.DNSError{Err: "no suitable address", Name: host}
	}

	fallback := d.fallbackDelay()
	if fallback < 0 {
		return d.dialSequential(ctx, network, port, candidates)
	}

	return d.dialHappyEyeballs(ctx, network, port, candidates, fallback)
}

func (d *Dialer) dial(ctx context.Context, network, address string) (net.Conn, error) {
	dialFn := d.dialFunc()
	return dialFn(ctx, network, address)
}

func (d *Dialer) dialFunc() DialFunc {
	if d != nil && d.Dial != nil {
		return d.Dial
	}
	dialer := &net.Dialer{}
	if d != nil && d.Base != nil {
		dialer = d.Base
	}
	return dialer.DialContext
}

func (d *Dialer) resolver() IPResolver {
	if d != nil && d.Resolver != nil {
		return d.Resolver
	}
	return net.DefaultResolver
}

func (d *Dialer) fallbackDelay() time.Duration {
	if d == nil {
		return DefaultFallbackDelay
	}
	if d.FallbackDelay == 0 {
		return DefaultFallbackDelay
	}
	return d.FallbackDelay
}

func (d *Dialer) dialSequential(ctx context.Context, network, port string, candidates []net.IPAddr) (net.Conn, error) {
	dialFn := d.dialFunc()
	var errs []error
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		address := net.JoinHostPort(candidate.String(), port)
		conn, err := dialFn(ctx, network, address)
		if err == nil {
			return conn, nil
		}
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		return nil, ctx.Err()
	}
	return nil, errors.Join(errs...)
}

func (d *Dialer) dialHappyEyeballs(ctx context.Context, network, port string, candidates []net.IPAddr, fallback time.Duration) (net.Conn, error) {
	dialFn := d.dialFunc()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type result struct {
		conn net.Conn
		err  error
	}

	results := make(chan result, len(candidates))
	var wg sync.WaitGroup

	for idx, candidate := range candidates {
		wg.Add(1)
		go func(i int, ipAddr net.IPAddr) {
			defer wg.Done()
			delay := time.Duration(i) * fallback
			if delay > 0 {
				timer := time.NewTimer(delay)
				defer timer.Stop()
				select {
				case <-ctx.Done():
					results <- result{err: ctx.Err()}
					return
				case <-timer.C:
				}
			} else {
				select {
				case <-ctx.Done():
					results <- result{err: ctx.Err()}
					return
				default:
				}
			}

			address := net.JoinHostPort(ipAddr.String(), port)
			conn, err := dialFn(ctx, network, address)
			if err != nil {
				results <- result{err: err}
				return
			}
			results <- result{conn: conn}
		}(idx, candidate)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	var errs []error
	for res := range results {
		if res.conn != nil {
			cancel()
			return res.conn, nil
		}
		if res.err != nil {
			errs = append(errs, res.err)
		}
	}

	if len(errs) == 0 {
		return nil, ctx.Err()
	}
	return nil, errors.Join(errs...)
}

func interleaveAddresses(primary, secondary []net.IPAddr, preferPrimary bool) []net.IPAddr {
	if !preferPrimary {
		primary, secondary = secondary, primary
	}
	maxLen := len(primary)
	if len(secondary) > maxLen {
		maxLen = len(secondary)
	}

	result := make([]net.IPAddr, 0, len(primary)+len(secondary))
	for i := 0; i < maxLen; i++ {
		if i < len(primary) {
			result = append(result, primary[i])
		}
		if i < len(secondary) {
			result = append(result, secondary[i])
		}
	}
	return result
}
