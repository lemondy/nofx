package vergex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// vergex.trade sits behind a Cloudflare managed challenge that only real
// browsers pass — the backend's direct requests get 403 ("Just a moment...").
// A headless Chromium (headless.go) fetches these endpoints in-page and relays
// the payloads here, letting strategy coin sources reuse browser-fresh data.
//
// The relay refreshes every minute, so an entry older than a couple of minutes
// means fetches are failing. Within relayTTL callers keep serving the last
// successful payload instead of erroring (vergex is the only source for AI500 /
// OI / NetFlow — there is no fallback). Past relayTTL the entry is dropped so
// strategies don't trade on an outdated candidate list, and the relay sends a
// Telegram alert at the same threshold (relayHealth in headless.go).
const relayTTL = 30 * time.Minute

var (
	relayMu    sync.Mutex
	relayCache = map[string]relayEntry{}
)

type relayEntry struct {
	body      []byte
	fetchedAt time.Time
	expires   time.Time
}

var relayNow = time.Now

// validatePayload distinguishes a real (possibly empty) ranking from an
// error object. JSON decoding alone accepts missing fields as empty slices.
func validatePayload(path string, body []byte) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return fmt.Errorf("vergex %s: expected response object", path)
	}
	if _, ok := obj["error"]; ok || bytes.Equal(bytes.TrimSpace(obj["success"]), []byte("false")) {
		return fmt.Errorf("vergex %s: upstream error payload", path)
	}
	array := func(m map[string]json.RawMessage, keys ...string) bool {
		found := false
		for _, key := range keys {
			if v, ok := m[key]; ok {
				v = bytes.TrimSpace(v)
				if len(v) == 0 || v[0] != '[' {
					return false
				}
				found = true
			}
		}
		return found
	}
	u, err := url.Parse(path)
	if err != nil {
		return fmt.Errorf("vergex invalid path: %w", err)
	}
	valid := false
	var shape any
	switch u.Path {
	case "/trending-category":
		var category map[string]json.RawMessage
		if raw, ok := obj["category"]; ok {
			if json.Unmarshal(raw, &category) != nil || !array(category, "assets") {
				return fmt.Errorf("vergex %s: invalid category", path)
			}
		}
		valid = array(category, "assets") || array(obj, "tableRows")
		if _, ok := obj["tableRows"]; ok && !array(obj, "tableRows") {
			valid = false
		}
		shape = &trendingCategoryResponse{}
	case "/trending-crypto":
		valid = array(obj, "top", "low")
		if u.Query().Get("tab") == "oi" {
			shape = &oiRowsResponse{}
		}
	case "/api/v1/data-intelligence/flow/markets":
		var data map[string]json.RawMessage
		valid = json.Unmarshal(obj["data"], &data) == nil && array(data, "inflow", "outflow")
		shape = &struct {
			Data struct {
				Inflow  []flowRow `json:"inflow"`
				Outflow []flowRow `json:"outflow"`
			} `json:"data"`
		}{}
	case "/trending-hl":
		valid = array(obj, "rows")
	}
	if !valid {
		return fmt.Errorf("vergex %s: missing or invalid ranking arrays", path)
	}
	if shape != nil {
		if err := json.Unmarshal(body, shape); err != nil {
			return fmt.Errorf("vergex %s: invalid ranking: %w", path, err)
		}
	}
	return nil
}

func feedRelayHTTP(path string, status int, body []byte) error {
	if status < 200 || status >= 300 {
		return fmt.Errorf("vergex %s: HTTP %d", path, status)
	}
	return FeedRelay(path, body)
}

// canonicalRelayKey reduces pathWithQuery to endpoint + identity params so
// kernel requests hit browser-relayed payloads regardless of parameter order
// or limit size: the browser relays e.g. "/trending-crypto?tab=oi&duration=1h&limit=100"
// while the kernel requests "/trending-crypto?duration=1h&limit=20&tab=oi" —
// same data, different shape. `limit` is dropped (clients slice after decode)
// and `lang` is dropped (symbol payloads are language-independent).
func canonicalRelayKey(pathWithQuery string) string {
	u, err := url.Parse(pathWithQuery)
	if err != nil {
		return pathWithQuery
	}
	q := u.Query()
	q.Del("limit")
	q.Del("lang")
	return u.Path + "?" + q.Encode()
}

// FeedRelay stores a vergex response body fetched by the app's browser for
// pathWithQuery (e.g. "/trending-category?key=ai500&lang=en").
func FeedRelay(pathWithQuery string, body []byte) error {
	if err := validatePayload(pathWithQuery, body); err != nil {
		return err
	}
	// The AI500 key is shared across consumers. Only a full-range payload may
	// populate it; a small browser request must not replace the full ranking.
	u, _ := url.Parse(pathWithQuery)
	if u.Path == "/trending-category" && u.Query().Get("key") == "ai500" {
		if limit, err := strconv.Atoi(u.Query().Get("limit")); err == nil && limit > 0 && limit < maxLimit {
			return fmt.Errorf("vergex AI500 relay requires full ranking (limit >= %d)", maxLimit)
		}
	}
	stored := make([]byte, len(body))
	copy(stored, body)

	key := canonicalRelayKey(pathWithQuery)
	relayMu.Lock()
	now := relayNow()
	relayCache[key] = relayEntry{body: stored, fetchedAt: now, expires: now.Add(relayTTL)}
	// Opportunistic cleanup so the map doesn't grow unbounded.
	if len(relayCache) > 64 {
		for k, e := range relayCache {
			if !now.Before(e.expires) {
				delete(relayCache, k)
			}
		}
	}
	relayMu.Unlock()
	return nil
}

// relayLookup returns a fresh relay-fed body for pathWithQuery (matched by
// canonical key — see canonicalRelayKey).
func relayLookup(pathWithQuery string) ([]byte, bool) {
	entry, ok := relayLookupEntry(pathWithQuery)
	return entry.body, ok
}

func relayLookupEntry(pathWithQuery string) (relayEntry, bool) {
	key := canonicalRelayKey(pathWithQuery)
	relayMu.Lock()
	entry, ok := relayCache[key]
	if ok && !relayNow().Before(entry.expires) {
		delete(relayCache, key)
		ok = false
	}
	relayMu.Unlock()
	if !ok {
		return relayEntry{}, false
	}
	return entry, true
}
