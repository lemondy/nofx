// Package pubcache coalesces and briefly caches public market GET responses.
// It is an HTTP overlay beneath existing caller caches: parsed-value and
// scanner caches retain their original TTLs, expiry rules, and parsing logic.
// A hit in those caches does not reach this package at all. Only requests
// that would otherwise reach the network can be shared or served here.
// Wrap must be OUTSIDE weight/traffic meters: Wrap(bnweight.Wrap(network, p)).
// All Wrap calls share one process-wide cache, including different clients.
package pubcache

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"nofx/logger"
)

// TTLs include network/guard latency: expiry is measured from fetch START,
// never extended by hits, completion, or waiting callers. There is no stale
// fallback. A caller's own cancellation/deadline is always respected.
const (
	KlineTTL        = 15 * time.Second
	OIHistoryTTL    = 60 * time.Second
	TickerTTL       = 30 * time.Second
	FundingTTL      = 10 * time.Minute
	PremiumIndexTTL = 10 * time.Second
)

// Exact path allowlist. Price/depth/bookTicker, exchangeInfo, listenKey and
// account/order endpoints are deliberately absent. Keep policy in this table.
var endpointTTLs = map[string]time.Duration{
	"/fapi/v1/klines":                KlineTTL,
	"/futures/data/openInterestHist": OIHistoryTTL,
	"/fapi/v1/ticker/24hr":           TickerTTL,
	"/fapi/v1/fundingRate":           FundingTTL,
	"/fapi/v1/premiumIndex":          PremiumIndexTTL,
	"/api/kline/list/open":           KlineTTL,
	"/api/kline/lists":               KlineTTL, // anonymous public variant only
}

// TTL returns the policy for an exact path; zero means bypass the cache.
func TTL(path string) time.Duration { return endpointTTLs[path] }

const (
	defaultMaxEntries = 2000
	defaultMaxBytes   = 64 << 20
	maxBodyBytes      = 8 << 20
	sweepInterval     = time.Minute
)

// Options configures an isolated cache. Now must be concurrency safe.
// Zero values select production defaults. No background goroutines are needed
// for isolated instances; expiry is cleaned lazily, including by Stats.
type Options struct {
	MaxEntries int
	MaxBytes   int
	Now        func() time.Time
}

type result struct {
	response http.Response // immutable after publication; Body/Request are nil
	body     []byte
	err      error
}

type entry struct {
	result  *result
	expires time.Time
}

type flight struct {
	done   chan struct{}
	result *result
}

// Counts separates stored hits, actual fetch attempts, and concurrent waiters.
// Requests = Hits + Misses + Coalesced; bypassed requests are excluded.
type Counts struct {
	Hits      uint64
	Misses    uint64
	Coalesced uint64
}

type Snapshot struct {
	Counts
	Entries   int
	Bytes     int
	Endpoints map[string]Counts
}

type Cache struct {
	mu         sync.Mutex
	entries    map[string]entry
	flights    map[string]*flight
	counts     map[string]Counts
	bytes      int
	maxEntries int
	maxBytes   int
	now        func() time.Time
	lastSweep  time.Time
}

func New(opts Options) *Cache {
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = defaultMaxEntries
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = defaultMaxBytes
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Cache{
		entries: make(map[string]entry), flights: make(map[string]*flight),
		counts: make(map[string]Counts), maxEntries: opts.MaxEntries,
		maxBytes: opts.MaxBytes, now: opts.Now,
	}
}

var shared = New(Options{})
var logOnce sync.Once

// Wrap shares both entries and in-flight work across clients and priorities.
// nil resolves the current http.DefaultTransport, preserving proxy/meter hooks.
func Wrap(base http.RoundTripper) http.RoundTripper {
	logOnce.Do(func() {
		go func() {
			for range time.NewTicker(10 * time.Minute).C {
				logger.Infof("[pubcache] %s", summary(Stats()))
			}
		}()
	})
	return shared.Wrap(base)
}

func Stats() Snapshot { return shared.Stats() }

func (c *Cache) Stats() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked(c.now())
	s := Snapshot{Entries: len(c.entries), Bytes: c.bytes, Endpoints: make(map[string]Counts, len(c.counts))}
	for path, n := range c.counts {
		s.Endpoints[path] = n
		s.Hits += n.Hits
		s.Misses += n.Misses
		s.Coalesced += n.Coalesced
	}
	return s
}

func summary(s Snapshot) string {
	paths := make([]string, 0, len(s.Endpoints))
	for path := range s.Endpoints {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	parts := []string{fmt.Sprintf("cumulative hits=%d misses=%d coalesced=%d entries=%d bytes=%d", s.Hits, s.Misses, s.Coalesced, s.Entries, s.Bytes)}
	for _, path := range paths {
		n := s.Endpoints[path]
		total := n.Hits + n.Misses + n.Coalesced
		parts = append(parts, fmt.Sprintf("%s hit=%.1f%% saved=%.1f%% (%d/%d/%d)", path,
			100*float64(n.Hits)/float64(total), 100*float64(n.Hits+n.Coalesced)/float64(total), n.Hits, n.Misses, n.Coalesced))
	}
	return strings.Join(parts, " ")
}

type transport struct {
	cache *Cache
	base  http.RoundTripper
}

func (c *Cache) Wrap(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if t, ok := base.(*transport); ok && t.cache == c {
		return t
	}
	return &transport{cache: c, base: base}
}

func requestKey(req *http.Request) (string, time.Duration) {
	if req.Method != http.MethodGet || req.URL == nil || req.URL.User != nil ||
		req.Body != nil && req.Body != http.NoBody || req.Host != "" && req.Host != req.URL.Host {
		return "", 0
	}
	u := *req.URL
	ttl := TTL(u.Path)
	if ttl == 0 || (u.Scheme != "https" && u.Scheme != "http") || u.Opaque != "" || u.RawPath != "" {
		return "", 0
	}
	// Even empty credential headers/parameters bypass; case-insensitive checks
	// also cover maps constructed directly rather than with Header.Set.
	for name := range req.Header {
		switch strings.ToLower(name) {
		case "x-mbx-apikey", "authorization", "proxy-authorization", "cookie", "apikey", "x-api-key", "signature":
			return "", 0
		}
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", 0
	}
	for name := range query {
		switch strings.ToLower(name) {
		case "signature", "listenkey", "apikey", "api_key", "timestamp", "recvwindow":
			return "", 0
		}
	}
	// Encode sorts parameter names. Preserve repeated-value order: upstreams
	// may interpret the first value, so reversing duplicates is not equivalent.
	u.RawQuery = query.Encode()
	u.Fragment, u.RawFragment = "", ""
	u.ForceQuery = false
	return req.Method + " " + u.String(), ttl
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	key, ttl := requestKey(req)
	if ttl == 0 {
		return t.base.RoundTrip(req)
	}
	c := t.cache
	c.mu.Lock()
	now := c.now()
	if now.Sub(c.lastSweep) >= sweepInterval {
		c.sweepLocked(now)
	}
	n := c.counts[req.URL.Path]
	if e, ok := c.entries[key]; ok {
		if now.Before(e.expires) {
			n.Hits++
			c.counts[req.URL.Path] = n
			c.mu.Unlock()
			return e.result.copyResponse(req)
		}
		c.deleteLocked(key)
	}
	if f, ok := c.flights[key]; ok {
		n.Coalesced++
		c.counts[req.URL.Path] = n
		c.mu.Unlock()
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-f.done:
			return f.result.copyResponse(req)
		}
	}
	f := &flight{done: make(chan struct{})}
	c.flights[key] = f
	n.Misses++
	c.counts[req.URL.Path] = n
	c.mu.Unlock()

	// The first caller owns the upstream request/deadline. Its failure is shared
	// with current waiters, then forgotten so the next request retries.
	r := fetch(t.base, req)
	cacheable := r.cacheable(req.URL.Path)
	expires := now.Add(ttl)
	if cacheable {
		expires = r.expiry(req, now, ttl)
	}
	// JSON inspection is outside the shared lock so different symbols/clients
	// can finish their upstream requests independently.
	c.mu.Lock()
	if cacheable && req.Context().Err() == nil && c.now().Before(expires) && len(r.body) <= c.maxBytes {
		c.sweepLocked(c.now())
		for len(c.entries) >= c.maxEntries || c.bytes+len(r.body) > c.maxBytes {
			c.evictLocked()
		}
		c.entries[key] = entry{result: r, expires: expires}
		c.bytes += len(r.body)
	}
	f.result = r
	delete(c.flights, key)
	close(f.done)
	c.mu.Unlock()
	return r.copyResponse(req)
}

func fetch(base http.RoundTripper, req *http.Request) *result {
	resp, err := base.RoundTrip(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		return &result{err: err}
	}
	if resp == nil || resp.Body == nil {
		return &result{err: errors.New("pubcache: missing response body")}
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	closeErr := resp.Body.Close()
	if readErr != nil {
		return &result{err: readErr}
	}
	if closeErr != nil {
		return &result{err: closeErr}
	}
	if len(body) > maxBodyBytes {
		return &result{err: errors.New("pubcache: response exceeds 8 MiB")}
	}
	if resp.ContentLength > 0 && int64(len(body)) != resp.ContentLength {
		return &result{err: io.ErrUnexpectedEOF}
	}
	if err := req.Context().Err(); err != nil {
		return &result{err: err}
	}
	r := &result{response: *resp, body: body}
	r.response.Body, r.response.Request = nil, nil
	r.response.Header = resp.Header.Clone()
	r.response.Trailer = resp.Trailer.Clone()
	r.response.TransferEncoding = nil
	r.response.ContentLength = int64(len(body))
	return r
}

func (r *result) cacheable(path string) bool {
	if r.err != nil || r.response.StatusCode != http.StatusOK || !json.Valid(r.body) {
		return false
	}
	// HTTP 200 error envelopes and invalid JSON must never poison the cache.
	// Binance array responses carry data; premiumIndex is an object. CoinAnk
	// explicitly reports application-level failures through success=false.
	var envelope map[string]json.RawMessage
	objectErr := json.Unmarshal(r.body, &envelope)
	if strings.HasPrefix(path, "/api/kline/") {
		return objectErr == nil && string(envelope["success"]) == "true" && envelope["data"] != nil
	}
	if objectErr != nil {
		return bytes.HasPrefix(bytes.TrimSpace(r.body), []byte("["))
	}
	return path == "/fapi/v1/premiumIndex" && envelope["symbol"] != nil && envelope["lastFundingRate"] != nil && envelope["code"] == nil && envelope["error"] == nil
}

// Never carry a cached forming candle across its close: callers judge closed
// bars using their own sampling clock, so an earlier partial OHLC must not
// suddenly become a final candle on a later hit. Historical closed-only series
// keep the normal TTL. This only SHORTENS the centralized TTL policy.
func (r *result) expiry(req *http.Request, started time.Time, ttl time.Duration) time.Time {
	expires := started.Add(ttl)
	if r.err != nil {
		return expires
	}
	var rows [][]json.RawMessage
	var closeColumn int
	switch req.URL.Path {
	case "/fapi/v1/klines":
		if json.Unmarshal(r.body, &rows) != nil {
			return expires
		}
		closeColumn = 6
	case "/api/kline/list/open", "/api/kline/lists":
		var envelope struct {
			Data [][]json.RawMessage `json:"data"`
		}
		if json.Unmarshal(r.body, &envelope) != nil {
			return expires
		}
		rows, closeColumn = envelope.Data, 1
	default:
		return expires
	}
	for _, row := range rows {
		if len(row) <= closeColumn {
			continue
		}
		closeMS, ok := decodeMillis(row[closeColumn])
		if !ok {
			continue
		}
		closeAt := time.UnixMilli(closeMS)
		if closeMS == 0 && closeColumn == 1 { // CoinAnk forming rows have end=0
			openMS, ok := decodeMillis(row[0])
			if !ok {
				continue
			}
			closeAt = candleEnd(time.UnixMilli(openMS), req.URL.Query().Get("interval"))
		}
		if !closeAt.Before(started) && closeAt.Before(expires) {
			expires = closeAt
		}
	}
	return expires
}

// CoinAnk models its rows as float64; accept decimal/exponent timestamp
// representations as well as integers, using the provider's rounding rule.
func decodeMillis(raw json.RawMessage) (int64, bool) {
	var ms float64
	if json.Unmarshal(raw, &ms) != nil || ms < 0 || ms >= float64(math.MaxInt64) {
		return 0, false
	}
	return int64(ms + 0.001), true
}

func candleEnd(open time.Time, interval string) time.Time {
	open = open.UTC()
	if len(interval) < 2 {
		return time.Time{}
	}
	n, err := strconv.Atoi(interval[:len(interval)-1])
	if err != nil || n <= 0 || n > 1000 {
		return time.Time{}
	}
	switch interval[len(interval)-1] {
	case 's':
		return open.Add(time.Duration(n) * time.Second)
	case 'm':
		return open.Add(time.Duration(n) * time.Minute)
	case 'h':
		return open.Add(time.Duration(n) * time.Hour)
	case 'd':
		return open.AddDate(0, 0, n)
	case 'w':
		return open.AddDate(0, 0, 7*n)
	case 'M':
		return open.AddDate(0, n, 0)
	case 'y':
		return open.AddDate(n, 0, 0)
	}
	return time.Time{}
}

func (r *result) copyResponse(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	if r.err != nil {
		return nil, r.err
	}
	resp := r.response
	resp.Header = r.response.Header.Clone()
	resp.Trailer = r.response.Trailer.Clone()
	resp.Body = io.NopCloser(bytes.NewReader(bytes.Clone(r.body)))
	resp.Request = req
	return &resp, nil
}

func (c *Cache) deleteLocked(key string) {
	c.bytes -= len(c.entries[key].result.body)
	delete(c.entries, key)
}

func (c *Cache) sweepLocked(now time.Time) {
	for key, e := range c.entries {
		if !now.Before(e.expires) {
			c.deleteLocked(key)
		}
	}
	c.lastSweep = now
}

func (c *Cache) evictLocked() {
	var oldest string
	var expires time.Time
	for key, e := range c.entries {
		if oldest == "" || e.expires.Before(expires) {
			oldest, expires = key, e.expires
		}
	}
	c.deleteLocked(oldest)
}
