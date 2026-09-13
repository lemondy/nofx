package vergex

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sync"
	"time"
)

// vergex.trade sits behind a Cloudflare managed challenge that only real
// browsers pass — the backend's direct requests get 403 ("Just a moment...").
// The web UI fetches these endpoints from the browser (CORS is open) and
// relays the payloads here, letting strategy coin sources reuse browser-fresh
// data. Entries older than relayTTL are ignored: candidate lists should not
// silently go stale, so the client falls back to a direct fetch (and the
// kernel falls back to NofxOS) instead.
const relayTTL = 10 * time.Minute

var (
	relayMu    sync.Mutex
	relayCache = map[string]relayEntry{}
)

type relayEntry struct {
	body    []byte
	expires time.Time
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
func FeedRelay(pathWithQuery string, body []byte) {
	if pathWithQuery == "" || len(body) == 0 || !json.Valid(body) {
		return
	}
	stored := make([]byte, len(body))
	copy(stored, body)

	key := canonicalRelayKey(pathWithQuery)
	relayMu.Lock()
	relayCache[key] = relayEntry{body: stored, expires: time.Now().Add(relayTTL)}
	// Opportunistic cleanup so the map doesn't grow unbounded.
	if len(relayCache) > 64 {
		now := time.Now()
		for k, e := range relayCache {
			if now.After(e.expires) {
				delete(relayCache, k)
			}
		}
	}
	relayMu.Unlock()
}

// relayLookup returns a fresh relay-fed body for pathWithQuery (matched by
// canonical key — see canonicalRelayKey).
func relayLookup(pathWithQuery string) ([]byte, bool) {
	key := canonicalRelayKey(pathWithQuery)
	relayMu.Lock()
	entry, ok := relayCache[key]
	if ok && time.Now().After(entry.expires) {
		delete(relayCache, key)
		ok = false
	}
	relayMu.Unlock()
	if !ok {
		return nil, false
	}
	return entry.body, true
}

// relayLookupJSON is fetchJSON's relay fast path: decode a fresh relay-fed
// body into out without touching the network.
func relayLookupJSON(pathWithQuery string, out interface{}) (bool, error) {
	body, ok := relayLookup(pathWithQuery)
	if !ok {
		return false, nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return true, fmt.Errorf("vergex JSON decode failed: %w", err)
	}
	return true, nil
}
