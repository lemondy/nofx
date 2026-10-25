package netmeter

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type countingResponseWriter struct {
	http.ResponseWriter
	written int64
}

func (w *countingResponseWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.written += int64(n)
	return n, err
}

func gzipForTest(t *testing.T, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func pathDownForTest(host, path string) int64 {
	for _, p := range SnapshotPaths(1000) {
		if p.Host == host && p.Path == path {
			return p.Down
		}
	}
	return 0
}

func TestRoundTripResponseCompression(t *testing.T) {
	plain := []byte(`{"symbols":[` + strings.TrimSuffix(strings.Repeat(`{"symbol":"BTCUSDT","status":"TRADING"},`, 20000), ",") + `]}`)
	compressed := gzipForTest(t, plain)
	if len(compressed) >= len(plain)/10 {
		t.Fatalf("test JSON did not compress enough: %d -> %d", len(plain), len(compressed))
	}
	for _, tt := range []struct {
		name, encoding, acceptEncoding string
		wireBody                       []byte
		decoded                        bool
	}{
		{"automatic-gzip", "gzip", "", compressed, true},
		{"mixed-case-gzip", "GZip", "", compressed, true},
		{"plain", "", "", plain, false},
		{"caller-gzip", "gzip", "gzip", compressed, false},
		{"caller-identity", "gzip", "identity", compressed, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			written := make(chan int64, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wantEncoding := tt.acceptEncoding
				if wantEncoding == "" {
					wantEncoding = "gzip"
				}
				if got := r.Header.Get("Accept-Encoding"); got != wantEncoding {
					t.Errorf("Accept-Encoding = %q, want %q", got, wantEncoding)
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-MBX-USED-WEIGHT-1M", "42")
				w.Header().Set("Content-Length", strconv.Itoa(len(tt.wireBody)))
				if tt.encoding != "" {
					w.Header().Set("Content-Encoding", tt.encoding)
				}
				cw := &countingResponseWriter{ResponseWriter: w}
				if _, err := cw.Write(tt.wireBody); err != nil {
					t.Errorf("write response: %v", err)
				}
				written <- cw.written
			}))
			defer srv.Close()

			path := "/wire-response/" + tt.name
			host := normalizeHost(srv.Listener.Addr().String())
			beforePath := pathDownForTest(host, path)
			_, _, beforeDown := totals()
			req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if tt.acceptEncoding != "" {
				req.Header.Set("Accept-Encoding", tt.acceptEncoding)
			}
			client := &http.Client{Transport: Wrap(Wrap(nil)), Timeout: 5 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.Uncompressed != tt.decoded {
				t.Fatalf("Uncompressed = %v, want %v", resp.Uncompressed, tt.decoded)
			}
			if tt.decoded {
				if resp.ContentLength != -1 || resp.Header.Get("Content-Encoding") != "" || resp.Header.Get("Content-Length") != "" {
					t.Fatalf("decoded response metadata: length=%d headers=%v", resp.ContentLength, resp.Header)
				}
				_, _, down := totals()
				if down != beforeDown {
					t.Fatalf("gzip bytes counted before Read: %d", down-beforeDown)
				}
			} else if resp.ContentLength != int64(len(tt.wireBody)) || resp.Header.Get("Content-Encoding") != tt.encoding {
				t.Fatalf("raw response metadata: length=%d headers=%v", resp.ContentLength, resp.Header)
			}
			if resp.Header.Get("X-MBX-USED-WEIGHT-1M") != "42" || resp.Header.Get("Content-Type") != "application/json" {
				t.Fatalf("unrelated response headers lost: %v", resp.Header)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			wantBody := tt.wireBody
			if tt.decoded {
				wantBody = plain
			}
			if !bytes.Equal(body, wantBody) {
				t.Fatalf("body differs: got %d bytes, want %d", len(body), len(wantBody))
			}
			wantDown := <-written
			_, _, down := totals()
			if got := down - beforeDown; got != wantDown {
				t.Fatalf("HTTPDown delta = %d, server wrote %d", got, wantDown)
			}
			if got := pathDownForTest(host, path) - beforePath; got != wantDown {
				t.Fatalf("path down delta = %d, server wrote %d", got, wantDown)
			}
		})
	}
}

func TestRoundTripRequestEncodingAndImmutability(t *testing.T) {
	for _, tt := range []struct {
		name, method string
		headers      http.Header
		wantEncoding string
	}{
		{"automatic", http.MethodGet, http.Header{"X-Test": {"first", "second"}}, "gzip"},
		{"nil-header", http.MethodGet, nil, "gzip"},
		{"caller-gzip", http.MethodGet, http.Header{"Accept-Encoding": {"gzip"}}, "gzip"},
		{"caller-identity", http.MethodGet, http.Header{"Accept-Encoding": {"identity"}}, "identity"},
		{"range", http.MethodGet, http.Header{"Range": {"bytes=0-3"}}, ""},
		{"head", http.MethodHead, http.Header{"X-Test": {"original"}}, ""},
		{"streamed-upload", http.MethodPost, http.Header{"X-Test": {"original"}}, "gzip"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Accept-Encoding"); got != tt.wantEncoding {
					t.Errorf("Accept-Encoding = %q, want %q", got, tt.wantEncoding)
				}
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					t.Errorf("read upload: %v", err)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()
			req, err := http.NewRequest(tt.method, srv.URL+"/request-encoding", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header = tt.headers.Clone()
			if tt.name == "streamed-upload" {
				req.Body = io.NopCloser(strings.NewReader("streamed request"))
				req.ContentLength = -1
			}
			before := *req
			before.Header = req.Header.Clone()
			resp, err := Wrap(nil).RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if !reflect.DeepEqual(req.Header, before.Header) || req.Body != before.Body || req.ContentLength != before.ContentLength {
				t.Fatalf("caller request mutated: headers %v -> %v, body %T -> %T", before.Header, req.Header, before.Body, req.Body)
			}
		})
	}
}

func TestRoundTripChunkedGzipStream(t *testing.T) {
	chunks := []string{"data: first\n\n", "data: second\n\n", "data: third\n\n"}
	advance := make(chan struct{}, len(chunks))
	written := make(chan int64, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cw := &countingResponseWriter{ResponseWriter: w}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		// Send headers without any gzip bytes. RoundTrip must return now,
		// before the client permits the first chunk to be sent.
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		zw := gzip.NewWriter(cw)
		for _, chunk := range chunks {
			select {
			case <-advance:
			case <-r.Context().Done():
				return
			}
			if _, err := io.WriteString(zw, chunk); err != nil {
				t.Errorf("write gzip chunk: %v", err)
				return
			}
			if err := zw.Flush(); err != nil {
				t.Errorf("flush gzip chunk: %v", err)
				return
			}
			w.(http.Flusher).Flush()
		}
		if err := zw.Close(); err != nil {
			t.Errorf("close gzip writer: %v", err)
		}
		written <- cw.written
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/gzip-stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, beforeDown := totals()
	resp, err := Wrap(nil).RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip must return before any gzip header arrives: %v", err)
	}
	defer resp.Body.Close()
	if !resp.Uncompressed || resp.ContentLength != -1 || resp.Header.Get("Content-Encoding") != "" ||
		len(resp.TransferEncoding) != 1 || resp.TransferEncoding[0] != "chunked" {
		t.Fatalf("unexpected streaming metadata: %+v", resp)
	}
	for _, chunk := range chunks {
		advance <- struct{}{}
		buf := make([]byte, len(chunk))
		if _, err := io.ReadFull(resp.Body, buf); err != nil {
			t.Fatalf("read chunk before permitting the next one: %v", err)
		}
		if string(buf) != chunk {
			t.Fatalf("chunk = %q, want %q", buf, chunk)
		}
	}
	if tail, err := io.ReadAll(resp.Body); err != nil || len(tail) != 0 {
		t.Fatalf("stream tail = %q, err = %v", tail, err)
	}
	_, _, down := totals()
	if want := <-written; down-beforeDown != want {
		t.Fatalf("chunked gzip HTTPDown delta = %d, server wrote %d", down-beforeDown, want)
	}
}

func TestRoundTripGzipHeaderErrorsAreLazy(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		wantErr    error
	}{
		{"empty", "", io.EOF},
		{"invalid", "not a gzip stream", gzip.ErrHeader},
		{"truncated", "\x1f\x8b", io.ErrUnexpectedEOF},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Encoding", "gzip")
				w.Header().Set("Content-Length", strconv.Itoa(len(tt.body)))
				io.WriteString(w, tt.body)
			}))
			defer srv.Close()
			client := &http.Client{Transport: Wrap(nil), Timeout: 5 * time.Second}
			_, _, beforeDown := totals()
			resp, err := client.Get(srv.URL + "/gzip-header-error")
			if err != nil {
				t.Fatalf("gzip header error escaped RoundTrip: %v", err)
			}
			defer resp.Body.Close()
			for i := 0; i < 2; i++ {
				n, err := resp.Body.Read(make([]byte, 32))
				if n != 0 || !errors.Is(err, tt.wantErr) {
					t.Fatalf("Read %d = %d, %v; want 0, %v", i, n, err, tt.wantErr)
				}
			}
			_, _, down := totals()
			if got := down - beforeDown; got != int64(len(tt.body)) {
				t.Fatalf("invalid gzip down = %d, want %d", got, len(tt.body))
			}
		})
	}
}

func TestRoundTripGzipCloseBeforeRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	client := &http.Client{Transport: Wrap(nil), Timeout: 5 * time.Second}
	_, _, beforeDown := totals()
	resp, err := client.Get(srv.URL + "/gzip-close-before-read")
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := resp.Body.Read(make([]byte, 1)); err == nil {
		t.Fatal("Read after Close succeeded")
	}
	_, _, down := totals()
	if down != beforeDown {
		t.Fatalf("closing unread gzip counted %d bytes", down-beforeDown)
	}
}

type observedReadCloser struct {
	io.ReadCloser
	reads, closes int
	closeErr      error
}

func (b *observedReadCloser) Read(p []byte) (int, error) {
	b.reads++
	return b.ReadCloser.Read(p)
}

func (b *observedReadCloser) Close() error {
	b.closes++
	b.ReadCloser.Close()
	return b.closeErr
}

// Exercise the body lifecycle independently of sockets, including transports
// that return a known Content-Length and errors from the underlying Close.
func TestRoundTripGzipBodyLifecycle(t *testing.T) {
	plain := []byte(strings.Repeat(`{"ok":true}`, 1000))
	compressed := gzipForTest(t, plain)
	for _, tt := range []struct {
		name    string
		encoded []byte
		wantErr error
		unread  bool
	}{
		{"valid", compressed, nil, false},
		{"invalid", []byte("not a gzip stream"), gzip.ErrHeader, false},
		{"empty", nil, nil, false}, // io.ReadAll turns the first Read's EOF into success
		{"truncated", []byte("\x1f\x8b"), io.ErrUnexpectedEOF, false},
		{"unread", compressed, nil, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			closeErr := errors.New("underlying close error")
			raw := &observedReadCloser{ReadCloser: io.NopCloser(bytes.NewReader(tt.encoded)), closeErr: closeErr}
			rt := Wrap(pathRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Header.Get("Accept-Encoding") != "gzip" {
					t.Fatal("netmeter did not request gzip")
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header: http.Header{
						"Content-Encoding": {"gzip"},
						"Content-Length":   {strconv.Itoa(len(tt.encoded))},
						"X-Test":           {"preserved"},
					},
					Body: raw, ContentLength: int64(len(tt.encoded)),
				}, nil
			}))
			req := httptest.NewRequest(http.MethodGet, "http://gzip-lifecycle.test/"+tt.name, nil)
			_, _, beforeDown := totals()
			resp, err := rt.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _, down := totals()
			if raw.reads != 0 || down != beforeDown {
				t.Fatal("RoundTrip read or counted the gzip body eagerly")
			}
			if !resp.Uncompressed || resp.ContentLength != -1 || resp.Header.Get("Content-Encoding") != "" ||
				resp.Header.Get("Content-Length") != "" || resp.Header.Get("X-Test") != "preserved" {
				t.Fatalf("unexpected gzip response metadata: %+v", resp)
			}
			if req.Header.Get("Accept-Encoding") != "" {
				t.Fatal("caller header mutated")
			}
			if !tt.unread {
				body, err := io.ReadAll(resp.Body)
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("ReadAll error = %v, want %v", err, tt.wantErr)
				}
				if tt.name == "valid" && !bytes.Equal(body, plain) {
					t.Fatal("decoded body differs")
				}
				reads := raw.reads
				if _, err := io.ReadAll(resp.Body); !errors.Is(err, tt.wantErr) || raw.reads != reads {
					t.Fatalf("repeated ReadAll: err=%v, raw reads %d -> %d", err, reads, raw.reads)
				}
			}
			if err := resp.Body.Close(); !errors.Is(err, closeErr) || raw.closes != 1 {
				t.Fatalf("Close = %v, underlying close calls = %d", err, raw.closes)
			}
			if _, err := resp.Body.Read(make([]byte, 1)); err == nil {
				t.Fatal("Read after Close succeeded")
			}
			wantDown := int64(len(tt.encoded))
			if tt.unread {
				wantDown = 0
				if raw.reads != 0 {
					t.Fatalf("closing unread gzip caused %d reads", raw.reads)
				}
			}
			_, _, down = totals()
			if down-beforeDown != wantDown {
				t.Fatalf("gzip body down = %d, want %d", down-beforeDown, wantDown)
			}
		})
	}
}

type blockingReadCloser struct {
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (b *blockingReadCloser) Read([]byte) (int, error) {
	close(b.started)
	<-b.closed
	return 0, http.ErrBodyReadAfterClose
}

func (b *blockingReadCloser) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func TestGzipCloseUnblocksRead(t *testing.T) {
	// Test both lazy header parsing and a decoder waiting for more stream data.
	for _, initialized := range []bool{false, true} {
		t.Run(strconv.FormatBool(initialized), func(t *testing.T) {
			raw := &blockingReadCloser{started: make(chan struct{}), closed: make(chan struct{})}
			var prefix bytes.Buffer
			if initialized {
				zw := gzip.NewWriter(&prefix)
				if _, err := zw.Write([]byte("first chunk")); err != nil {
					t.Fatal(err)
				}
				if err := zw.Flush(); err != nil {
					t.Fatal(err)
				}
			}
			body := &gzipReadCloser{body: struct {
				io.Reader
				io.Closer
			}{io.MultiReader(bytes.NewReader(prefix.Bytes()), raw), raw}}
			defer body.Close()
			readDone := make(chan error, 1)
			go func() {
				_, err := io.Copy(io.Discard, body)
				readDone <- err
			}()
			select {
			case <-raw.started:
			case <-time.After(5 * time.Second):
				t.Fatal("Read never reached the underlying body")
			}
			closeDone := make(chan error, 1)
			go func() { closeDone <- body.Close() }()
			select {
			case err := <-readDone:
				if err == nil {
					t.Fatal("Read of unfinished gzip stream succeeded")
				}
			case <-time.After(5 * time.Second):
				// Unblock the test even if Close holds the decoder lock first.
				raw.Close()
				t.Fatal("Close did not unblock Read")
			}
			select {
			case err := <-closeDone:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Close did not finish")
			}
		})
	}
}
