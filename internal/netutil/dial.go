package netutil

import (
	"context"
	"net"
	"net/url"
	"strings"
)

// NormalizeResolveAddress normalizes resolve target input and supports:
// - host
// - host:port
// - https://host[:port][/path]
// - host[/path]
// It also removes trailing dot from FQDN.
func NormalizeResolveAddress(input string) string {
	v := strings.TrimSpace(input)
	if v == "" {
		return ""
	}

	if strings.Contains(v, "://") {
		if u, err := url.Parse(v); err == nil && u.Host != "" {
			v = u.Host
		}
	}

	if i := strings.Index(v, "/"); i >= 0 {
		v = v[:i]
	}
	v = strings.TrimSpace(v)
	v = strings.TrimSuffix(v, ".")
	return v
}

// BuildResolveDialContext returns a DialContext that redirects connections for targetHost
// to resolveAddress (host or host:port). If resolveAddress is empty, it returns dialer.DialContext.
func BuildResolveDialContext(dialer *net.Dialer, targetHost, resolveAddress string) func(context.Context, string, string) (net.Conn, error) {
	targetHost = strings.TrimSuffix(strings.TrimSpace(strings.ToLower(targetHost)), ".")
	resolveAddress = NormalizeResolveAddress(resolveAddress)
	if targetHost == "" || resolveAddress == "" {
		return dialer.DialContext
	}

	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return dialer.DialContext(ctx, network, addr)
		}
		if !strings.EqualFold(host, targetHost) {
			return dialer.DialContext(ctx, network, addr)
		}

		overrideAddr := resolveAddress
		if _, _, splitErr := net.SplitHostPort(resolveAddress); splitErr != nil {
			overrideAddr = net.JoinHostPort(resolveAddress, port)
		}
		return dialer.DialContext(ctx, network, overrideAddr)
	}
}
