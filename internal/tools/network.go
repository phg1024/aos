package tools

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"aos/internal/capability"
)

// sharedAddressSpace is 100.64.0.0/10 (carrier-grade NAT, also Tailscale),
// which net.IP.IsPrivate does not cover.
var sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10")

// fetchClient is web_fetch's HTTP client. Every connection, redirects
// included, is checked where it is dialled: the host must not be the model
// endpoint, and every address it resolves to must be public, unless the
// capability set's NetworkAllow names the host, address or a range
// containing it. The checked address is the one dialled, so a name cannot
// resolve differently between check and connect. Proxies are not used.
func (r *Runtime) fetchClient(set capability.Set, timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: timeout}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			if allowedHost(set.NetworkAllow, host) {
				return dialer.DialContext(ctx, network, address)
			}
			if r.isModelHost(host) {
				return nil, fmt.Errorf("%w: %s is the model endpoint", ErrNetworkDenied, host)
			}
			addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
			var lastErr error = fmt.Errorf("%w: no public address for %s", ErrNetworkDenied, host)
			for _, ip := range addresses {
				ip = ip.Unmap()
				if !publicAddress(ip) && !allowedAddress(set.NetworkAllow, ip) {
					lastErr = fmt.Errorf("%w: %s resolves to non-public %s", ErrNetworkDenied, host, ip)
					continue
				}
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			return nil, lastErr
		},
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}

func (r *Runtime) isModelHost(host string) bool {
	for _, model := range r.ModelHosts {
		if strings.EqualFold(strings.Trim(model, "[]"), strings.Trim(host, "[]")) {
			return true
		}
	}
	return false
}

// publicAddress reports whether ip is an ordinary internet address: not
// loopback, private, link-local, shared (CGNAT), multicast or unspecified.
func publicAddress(ip netip.Addr) bool {
	return ip.IsValid() && !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() &&
		!ip.IsLinkLocalMulticast() && !ip.IsInterfaceLocalMulticast() && !ip.IsMulticast() &&
		!ip.IsUnspecified() && !sharedAddressSpace.Contains(ip)
}

func allowedHost(allow []string, host string) bool {
	host = strings.Trim(host, "[]")
	for _, entry := range allow {
		if strings.EqualFold(entry, host) {
			return true
		}
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return allowedAddress(allow, ip.Unmap())
	}
	return false
}

func allowedAddress(allow []string, ip netip.Addr) bool {
	for _, entry := range allow {
		if prefix, err := netip.ParsePrefix(entry); err == nil && prefix.Contains(ip) {
			return true
		}
		if addr, err := netip.ParseAddr(entry); err == nil && addr.Unmap() == ip {
			return true
		}
	}
	return false
}
