// Package security provides security utilities for the application
package security

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Private/Reserved IP ranges that should be blocked to prevent SSRF
var privateIPBlocks []*net.IPNet

func init() {
	// Initialize private IP blocks
	// These ranges should not be accessible via user-controlled URLs
	privateRanges := []string{
		"127.0.0.0/8",    // IPv4 loopback
		"10.0.0.0/8",     // RFC1918 private
		"172.16.0.0/12",  // RFC1918 private
		"192.168.0.0/16", // RFC1918 private
		"169.254.0.0/16", // Link-local / Cloud metadata
		"0.0.0.0/8",      // Current network
		"224.0.0.0/4",    // Multicast
		"240.0.0.0/4",    // Reserved
		"::1/128",        // IPv6 loopback
		"fe80::/10",      // IPv6 link-local
		"fc00::/7",       // IPv6 unique local
	}

	for _, cidr := range privateRanges {
		_, block, err := net.ParseCIDR(cidr)
		if err == nil {
			privateIPBlocks = append(privateIPBlocks, block)
		}
	}
}

// SSRFError represents a Server-Side Request Forgery attempt
type SSRFError struct {
	URL    string
	Reason string
}

func (e *SSRFError) Error() string {
	return fmt.Sprintf("SSRF blocked: %s - %s", e.URL, e.Reason)
}

// isPrivateIP checks if an IP address is in a private/reserved range
func isPrivateIP(ip net.IP) bool {
	if ip == nil {
		return true // Invalid IP, treat as private
	}

	// Check if it's a loopback address
	if ip.IsLoopback() {
		return true
	}

	// Check if it's a link-local address
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}

	// Check if it's a private address
	if ip.IsPrivate() {
		return true
	}

	// Check against our explicit private ranges
	for _, block := range privateIPBlocks {
		if block.Contains(ip) {
			return true
		}
	}

	return false
}

// lookupIPAddr is swappable in tests.
var lookupIPAddr = func(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

var dnsRetryDelay = 300 * time.Millisecond

// trustedPublicDomains are the built-in upstreams this application ships
// with. A host matches when it equals a domain or is a subdomain of it.
var trustedPublicDomains = []string{
	// AI providers
	"bigmodel.cn", "z.ai", "deepseek.com", "openai.com", "anthropic.com",
	"x.ai", "moonshot.ai", "moonshot.cn", "minimax.io", "dashscope.aliyuncs.com",
	"generativelanguage.googleapis.com",
	// exchanges
	"binance.com", "hyperliquid.xyz", "bybit.com", "okx.com", "bitget.com",
	"gateio.ws", "gate.io", "kucoin.com", "asterdex.com", "zklighter.elliot.ai",
	// market data
	"nofxos.ai", "coinank.com", "alternative.me", "twelvedata.com", "alpaca.markets",
}

func isTrustedPublicHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, d := range trustedPublicDomains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// ValidateURL checks if a URL is safe to request (not pointing to internal networks)
// Returns an error if the URL is potentially dangerous
func ValidateURL(rawURL string) error {
	if rawURL == "" {
		return &SSRFError{URL: rawURL, Reason: "empty URL"}
	}

	// Parse the URL
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return &SSRFError{URL: rawURL, Reason: "invalid URL format"}
	}

	// Only allow http and https schemes
	scheme := strings.ToLower(parsedURL.Scheme)
	if scheme != "http" && scheme != "https" {
		return &SSRFError{URL: rawURL, Reason: fmt.Sprintf("unsupported scheme: %s", scheme)}
	}

	// Extract hostname (without port)
	host := parsedURL.Hostname()
	if host == "" {
		return &SSRFError{URL: rawURL, Reason: "empty hostname"}
	}

	// Block localhost and common internal hostnames
	lowerHost := strings.ToLower(host)
	blockedHosts := []string{
		"localhost",
		"127.0.0.1",
		"::1",
		"0.0.0.0",
		"metadata.google.internal",
		"metadata.google",
		"instance-data",
	}
	for _, blocked := range blockedHosts {
		if lowerHost == blocked {
			return &SSRFError{URL: rawURL, Reason: fmt.Sprintf("blocked hostname: %s", host)}
		}
	}

	// Resolve the hostname to IP addresses
	// This catches DNS rebinding and ensures we check the actual destination
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ips, err := lookupIPAddr(ctx, host)
	if err != nil && hasEnvProxy() && net.ParseIP(host) == nil {
		// One short retry: a transient local DNS miss behind a proxy used to
		// fail the whole AI decision cycle (2026-10-07 review N3: 88 lost
		// cycles in 3 days on open.bigmodel.cn).
		time.Sleep(dnsRetryDelay)
		ips, err = lookupIPAddr(ctx, host)
	}
	if err != nil {
		// If DNS resolution fails, we still need to check if it's an IP address directly
		ip := net.ParseIP(host)
		if ip != nil {
			if isPrivateIP(ip) {
				return &SSRFError{URL: rawURL, Reason: "resolves to private IP address"}
			}
			return nil // It's a valid public IP
		}
		// DNS resolution failed and it's not an IP literal. Direct dial:
		// let the HTTP client surface the error. WITH a proxy configured the
		// destination check is skipped at dial time — a locally
		// unresolvable name could resolve to a private IP on the proxy's
		// side, so fail closed (2026-10-03 review P2).
		if hasEnvProxy() {
			// Built-in public API endpoints (AI providers, exchanges, market
			// data) are not user-chosen destinations; their public DNS names
			// cannot be steered to an internal address by the caller, so a
			// local resolver hiccup must not block them. User-supplied hosts
			// stay fail-closed.
			if isTrustedPublicHost(lowerHost) {
				return nil
			}
			return &SSRFError{URL: rawURL, Reason: "DNS resolution failed while a proxy is configured — destination cannot be verified"}
		}
		return nil
	}

	// Check all resolved IPs
	for _, ipAddr := range ips {
		if isPrivateIP(ipAddr.IP) {
			return &SSRFError{URL: rawURL, Reason: fmt.Sprintf("resolves to private IP: %s", ipAddr.IP)}
		}
	}

	return nil
}

// SafeHTTPClient returns an HTTP client with SSRF protection
// It validates URLs and blocks requests to private networks.
// It also honors the standard HTTP(S)_PROXY / ALL_PROXY / NO_PROXY
// environment variables, so deployments behind a local proxy (required to
// reach Binance and other blocked upstreams from some networks) work out of
// the box.
func SafeHTTPClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout:   timeout,
		KeepAlive: 30 * time.Second,
	}

	proxyHosts := envProxyHosts()

	transport := &http.Transport{
		Proxy: func(req *http.Request) (*url.URL, error) {
			if err := ValidateURL(req.URL.String()); err != nil {
				return nil, err
			}
			return http.ProxyFromEnvironment(req)
		},
		// Re-enable HTTP/2: a custom DialContext alone would force HTTP/1.1.
		ForceAttemptHTTP2: true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// Extract host from address
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				host = addr
			}

			// When a proxy is in use, the dial target is the proxy itself —
			// an operator-chosen egress whose address must not trip the
			// private-IP check (the real destination is resolved remotely by
			// the proxy, so dial-time DNS validation cannot apply anyway).
			if !proxyHosts[host] {
				ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
				if err != nil {
					return nil, err
				}
				for _, ip := range ips {
					if isPrivateIP(ip.IP) {
						return nil, fmt.Errorf("SSRF protection: private IP %s", ip.IP)
					}
				}
				_, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				var dialErr error
				for _, ip := range ips {
					conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
					if err == nil {
						return conn, nil
					}
					dialErr = err
				}
				if dialErr == nil {
					dialErr = fmt.Errorf("SSRF protection: no resolved address for %s", host)
				}
				return nil, dialErr
			}

			return dialer.DialContext(ctx, network, addr)
		},
	}

	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}

			// Validate the redirect URL
			if err := ValidateURL(req.URL.String()); err != nil {
				return fmt.Errorf("SSRF protection: redirect blocked - %w", err)
			}

			return nil
		},
	}
}

// envProxyHosts returns the hostnames of the proxies configured through the
// standard environment variables, so DialContext can recognize (and exempt)
// dials that target the proxy itself.
func envProxyHosts() map[string]bool {
	hosts := map[string]bool{}
	for _, key := range []string{
		"HTTP_PROXY", "http_proxy",
		"HTTPS_PROXY", "https_proxy",
		"ALL_PROXY", "all_proxy",
	} {
		raw := os.Getenv(key)
		if raw == "" {
			continue
		}
		// Schemeless values ("127.0.0.1:7890") are accepted by the httpproxy
		// parser behind http.ProxyFromEnvironment — the transport dials the
		// proxy — but url.Parse REJECTS them ("first path segment cannot
		// contain colon"), which left this exemption list empty and the SSRF
		// guard blocking the operator's own proxy as a private IP (09-30:
		// every direct-Binance fetch failed, the candidate pool collapsed to
		// the single universe source that bypasses fapi). Normalize before
		// parsing so the exemption list always matches what the transport
		// actually dials.
		if !strings.Contains(raw, "://") {
			raw = "http://" + raw
		}
		if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
			hosts[u.Hostname()] = true
		}
	}
	return hosts
}

// SafeGet performs a GET request with SSRF protection
// It validates the URL before making the request and uses a safe HTTP client
func SafeGet(rawURL string, timeout time.Duration) (*http.Response, error) {
	// First validate the URL
	if err := ValidateURL(rawURL); err != nil {
		return nil, err
	}

	// Use the safe HTTP client
	client := SafeHTTPClient(timeout)
	return client.Get(rawURL)
}

// hasEnvProxy reports whether a standard-environment proxy is configured
// (the same variables envProxyHosts reads).
func hasEnvProxy() bool {
	for _, key := range []string{
		"HTTP_PROXY", "http_proxy",
		"HTTPS_PROXY", "https_proxy",
		"ALL_PROXY", "all_proxy",
	} {
		if os.Getenv(key) != "" {
			return true
		}
	}
	return false
}
