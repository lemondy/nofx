// Package netmeter counts the process's outbound network usage for the
// settings-page traffic monitor: cumulative HTTP payload bytes (request +
// response bodies) per host since process start, plus wire-level bytes for
// raw connections (websocket feeds) where per-host attribution at the HTTP
// layer does not exist.
//
// Counting is deliberately at the RoundTripper layer, not the socket layer:
// behind the local proxy a socket dial only ever targets 127.0.0.1:7890, so
// the real destination host is only known to the HTTP layer. HTTP header and
// TLS overhead are therefore excluded from the HTTP numbers — the UI states
// this so nobody reads the total as a proxy-quota wire count.
//
// All public entry points are safe for concurrent use.
package netmeter

import (
	"context"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var startedAt = time.Now()

type hostStats struct {
	requests atomic.Int64
	up       atomic.Int64 // request payload bytes
	down     atomic.Int64 // response payload bytes
}

var (
	hosts     sync.Map // host (no port) -> *hostStats
	totalReqs atomic.Int64
	totalUp   atomic.Int64
	totalDown atomic.Int64

	// Wire bytes for raw connections (websocket feeds). Includes TLS and
	// framing overhead, so they are reported separately from HTTP payload
	// counters instead of being mixed into them.
	wsUp   atomic.Int64
	wsDown atomic.Int64
)

func hostFor(hostPort string) *hostStats {
	host := strings.ToLower(hostPort)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if v, ok := hosts.Load(host); ok {
		return v.(*hostStats)
	}
	v, _ := hosts.LoadOrStore(host, &hostStats{})
	return v.(*hostStats)
}

func addUp(h *hostStats, n int64) {
	if n <= 0 {
		return
	}
	h.up.Add(n)
	totalUp.Add(n)
}

func addDown(h *hostStats, n int64) {
	if n <= 0 {
		return
	}
	h.down.Add(n)
	totalDown.Add(n)
}

// transport counts payload bytes flowing through base.
type transport struct {
	base http.RoundTripper
}

// Wrap installs payload counting on base (nil = http.DefaultTransport).
// Any existing counting layer inside base is unwrapped first, so nesting
// (e.g. Wrap over an already-swapped DefaultTransport, or bnweight over
// netmeter over default) never double-counts.
func Wrap(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	for {
		t, ok := base.(*transport)
		if !ok {
			break
		}
		base = t.base
	}
	return &transport{base: base}
}

func init() {
	// Bare &http.Client{} (no Transport) resolves http.DefaultTransport at
	// request time; swapping it here covers every such client in the process.
	http.DefaultTransport = Wrap(http.DefaultTransport)
}

// Unwrap strips any counting layer from rt, returning the transport beneath
// (for tests and introspection). Returns rt itself when not wrapped.
func Unwrap(rt http.RoundTripper) http.RoundTripper {
	for {
		t, ok := rt.(*transport)
		if !ok {
			return rt
		}
		rt = t.base
	}
}

// countingReadCloser tallies bytes as the consumer reads them; used when the
// length is unknown ahead of time (chunked bodies).
type countingReadCloser struct {
	io.ReadCloser
	onRead func(int64)
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	if n > 0 {
		c.onRead(int64(n))
	}
	return n, err
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	h := hostFor(req.URL.Host)
	h.requests.Add(1)
	totalReqs.Add(1)

	if req.Body != nil {
		if req.ContentLength > 0 {
			addUp(h, req.ContentLength)
		} else {
			// Length unknown (net/http leaves ContentLength at 0 for streamed
			// bodies, not -1): count as the transport reads it. A truly empty
			// body simply contributes 0.
			req.Body = &countingReadCloser{ReadCloser: req.Body, onRead: func(n int64) { addUp(h, n) }}
		}
	}

	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	if resp.Body != nil {
		if resp.ContentLength > 0 {
			addDown(h, resp.ContentLength)
		} else {
			resp.Body = &countingReadCloser{ReadCloser: resp.Body, onRead: func(n int64) { addDown(h, n) }}
		}
	}
	return resp, nil
}

// countingConn tallies wire bytes on a raw connection.
type countingConn struct {
	net.Conn
}

func (c *countingConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		wsDown.Add(int64(n))
	}
	return n, err
}

func (c *countingConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 {
		wsUp.Add(int64(n))
	}
	return n, err
}

var wireDialer = &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}

// NetDialContext is a drop-in dialer for websocket libraries that dial raw
// connections (optionally through the local proxy — gorilla dials the proxy
// itself through this hook, so tunneled TLS bytes are still captured).
func NetDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	conn, err := wireDialer.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	return &countingConn{Conn: conn}, nil
}

// HostUsage is one host's cumulative HTTP usage.
type HostUsage struct {
	Host     string `json:"host"`
	Requests int64  `json:"requests"`
	Up       int64  `json:"up"`
	Down     int64  `json:"down"`
}

// SnapshotData is the process-lifetime network usage summary served by
// GET /api/net-traffic.
type SnapshotData struct {
	Since    time.Time `json:"since"`
	Requests int64     `json:"requests"`
	HTTPUp   int64     `json:"http_up"`   // payload bytes, header/TLS excluded
	HTTPDown int64     `json:"http_down"` // payload bytes, header/TLS excluded
	WsUp     int64     `json:"ws_up"`     // raw-conn wire bytes (feeds)
	WsDown   int64     `json:"ws_down"`   // raw-conn wire bytes (feeds)
}

// Snapshot returns the totals plus per-host usage sorted by total bytes desc.
func Snapshot() (SnapshotData, []HostUsage) {
	data := SnapshotData{
		Since:    startedAt,
		Requests: totalReqs.Load(),
		HTTPUp:   totalUp.Load(),
		HTTPDown: totalDown.Load(),
		WsUp:     wsUp.Load(),
		WsDown:   wsDown.Load(),
	}
	list := make([]HostUsage, 0, 8)
	hosts.Range(func(key, v any) bool {
		hs := v.(*hostStats)
		list = append(list, HostUsage{
			Host:     key.(string),
			Requests: hs.requests.Load(),
			Up:       hs.up.Load(),
			Down:     hs.down.Load(),
		})
		return true
	})
	sort.Slice(list, func(i, j int) bool {
		return list[i].Up+list[i].Down > list[j].Up+list[j].Down
	})
	return data, list
}
