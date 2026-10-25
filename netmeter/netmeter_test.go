package netmeter

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// counting is process-global, so every test reads deltas around its own
// traffic instead of absolute values.
func totals() (reqs, up, down int64) {
	d, _ := Snapshot()
	return d.Requests, d.HTTPUp, d.HTTPDown
}

func TestRoundTripCountsKnownLengthBodies(t *testing.T) {
	const reqBody = `{"q":"ping"}`
	const respBody = `{"ok":true}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Length", strconv.Itoa(len(respBody)))
		w.Write([]byte(respBody))
	}))
	defer srv.Close()

	client := &http.Client{Transport: Wrap(http.DefaultTransport)}
	beforeReqs, beforeUp, beforeDown := totals()
	resp, err := client.Post(srv.URL+"/x", "application/json", bytes.NewReader([]byte(reqBody)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	reqs, up, down := totals()
	if reqs-beforeReqs != 1 {
		t.Fatalf("requests counted = %d, want 1", reqs-beforeReqs)
	}
	// up = request line ("/x") + body
	if want := int64(len("/x") + len(reqBody)); up-beforeUp != want {
		t.Fatalf("up = %d, want %d", up-beforeUp, want)
	}
	if got := down - beforeDown; got != int64(len(respBody)) {
		t.Fatalf("down = %d, want %d", got, len(respBody))
	}
}

func TestRoundTripCountsUnknownLengthBodies(t *testing.T) {
	respBody := bytes.Repeat([]byte("x"), 5000) // flushed chunked: ContentLength -1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Write(respBody)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer srv.Close()

	client := &http.Client{Transport: Wrap(http.DefaultTransport)}
	_, beforeUp, beforeDown := totals()
	// io.Pipe: unknown request length (-1) → counted via wrapped reader
	pr, pw := io.Pipe()
	go func() {
		pw.Write(bytes.Repeat([]byte("y"), 700))
		pw.Close()
	}()
	resp, err := client.Post(srv.URL+"/y", "text/plain", pr)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	_, up, down := totals()
	if want := int64(700 + len("/y")); up-beforeUp != want {
		t.Fatalf("up = %d, want %d", up-beforeUp, want)
	}
	if got := down - beforeDown; got != int64(len(respBody)) {
		t.Fatalf("down = %d, want %d (chunked must be counted as read)", got, len(respBody))
	}
}

func TestWrapIdempotentNoDoubleCount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("abcd"))
	}))
	defer srv.Close()

	// Wrap(nil) resolves http.DefaultTransport, which init() already swapped
	// for a counting transport; wrapping that again must still yield exactly
	// one counting layer.
	client := &http.Client{Transport: Wrap(Wrap(nil))}
	beforeReqs, _, beforeDown := totals()
	resp, err := client.Get(srv.URL + "/z")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	reqs, _, down := totals()
	if reqs-beforeReqs != 1 {
		t.Fatalf("requests counted = %d, want 1 (no double count)", reqs-beforeReqs)
	}
	if got := down - beforeDown; got != 4 {
		t.Fatalf("down = %d, want 4 (no double count)", got)
	}
}

func TestHostAttributionAndSnapshot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("12345"))
	}))
	defer srv.Close()

	client := &http.Client{Transport: Wrap(http.DefaultTransport)}
	resp, _ := client.Get(srv.URL + "/h")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	host := srv.Listener.Addr().String() // host:port — Snapshot keys must be port-stripped
	_, list := Snapshot()
	found := false
	for _, h := range list {
		if h.Host == stripPortForTest(host) {
			found = true
			if h.Requests < 1 || h.Down < 5 {
				t.Fatalf("host %s usage = %+v, want requests>=1 down>=5", h.Host, h)
			}
		}
	}
	if !found {
		t.Fatalf("host %s missing from snapshot: %+v", stripPortForTest(host), list)
	}
}

func TestSnapshotSinceIsProcessStart(t *testing.T) {
	data, _ := Snapshot()
	if data.Since.IsZero() {
		t.Fatal("Since must be captured at process start")
	}
	if time.Since(data.Since) < 0 {
		t.Fatal("Since is in the future")
	}
}

func TestCountingConnWireBytes(t *testing.T) {
	// Dial a real loopback listener through NetDialContext and move bytes in
	// both directions; the wire counters must grow by exactly that much.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		buf := make([]byte, 5)
		io.ReadFull(c, buf)
		c.Write([]byte("pong"))
		c.Close()
	}()

	beforeUp, beforeDown := wireTotals()
	conn, err := NetDialContext(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	n, _ := conn.Read(buf)
	conn.Close()
	if n != 4 {
		t.Fatalf("read %d bytes, want 4 (echo must be delivered)", n)
	}
	up, down := wireTotals()
	if got := up - beforeUp; got != 5 {
		t.Fatalf("ws up = %d, want 5", got)
	}
	if got := down - beforeDown; got != 4 {
		t.Fatalf("ws down = %d, want 4", got)
	}
}

func wireTotals() (up, down int64) {
	d, _ := Snapshot()
	return d.WsUp, d.WsDown
}

func stripPortForTest(h string) string {
	if i := bytes.LastIndexByte([]byte(h), ':'); i >= 0 {
		return h[:i]
	}
	return h
}
