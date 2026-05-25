package network

import (
	"context"
	"fmt"
	"net"
	"time"
)

// Public DNS resolvers we'll dial directly, bypassing systemd-resolved.
// Two-vendor redundancy is the load-bearing property — under
// cargocam/ghostcam#132's failure mode the cellular APN's path to a
// single resolver intermittently collapses (BICS roaming + 8.8.4.4
// observed cascading through UDP+EDNS0 → UDP → TCP → UDP and never
// converging). Trying Cloudflare AND Google in sequence makes us
// resilient to one provider's path going dark.
var publicDNSResolvers = []string{
	"1.1.1.1:53", // Cloudflare primary
	"8.8.8.8:53", // Google primary
	"1.0.0.1:53", // Cloudflare secondary
	"8.8.4.4:53", // Google secondary
}

const (
	dnsDialTimeout    = 3 * time.Second
	dnsPerQueryBudget = 5 * time.Second
)

// CellularAwareResolver returns a net.Resolver that bypasses
// /etc/resolv.conf (and therefore systemd-resolved) by dialing one
// of `publicDNSResolvers` directly. The intent is documented in
// cargocam/ghostcam#132: on a cellular link, systemd-resolved
// occasionally enters a stuck "degraded feature" cascade (UDP+EDNS0
// → UDP → TCP → UDP) that produces ten-minute DNS outages while
// the underlying network is otherwise healthy. The Go runtime's
// default net.Resolver delegates to that cascade via the cgo
// resolver / glibc / systemd-resolved chain; we sidestep the whole
// pile by speaking DNS-over-UDP straight to a known-good upstream.
//
// PreferGo:true forces the pure-Go resolver path, which is what
// makes the custom Dial function actually get invoked — without it
// glibc gets first crack and Dial is bypassed.
//
// Each DNS query has its own dialDeadline so a single dead upstream
// doesn't burn the whole query budget. Failures fall through to the
// next resolver in the list.
func CellularAwareResolver() *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			// `network` is "udp" or "tcp"; address is whatever
			// /etc/resolv.conf had — we ignore it. Try each known
			// public resolver in sequence with a bounded per-dial
			// timeout, returning the first successful connection.
			d := net.Dialer{Timeout: dnsDialTimeout}
			var lastErr error
			for _, server := range publicDNSResolvers {
				dctx, cancel := context.WithTimeout(ctx, dnsDialTimeout)
				conn, err := d.DialContext(dctx, network, server)
				cancel()
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			if lastErr == nil {
				lastErr = fmt.Errorf("no public DNS resolvers configured")
			}
			return nil, fmt.Errorf("all DNS resolvers unreachable: %w", lastErr)
		},
	}
}
