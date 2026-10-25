package netmeter

import (
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	maxPaths      = 200
	maxPathLength = 120
	otherPath     = "/(other)"
)

// normalizePath keeps static endpoint names while bounding cardinality and
// removing credentials. Escaped segments are decoded only for classification;
// unchanged segments retain their escaping (including encoded slashes).
func normalizePath(escapedPath string) string {
	if i := strings.IndexAny(escapedPath, "?#"); i >= 0 {
		escapedPath = escapedPath[:i]
	}
	if escapedPath == "" {
		return "/"
	}
	// Allocate only when a segment changes. Most exchange API paths can be
	// returned directly, without allocating or compiling/running a regexp.
	var b strings.Builder
	copied := 0
	for start := 0; start < len(escapedPath) && b.Len()+start-copied < maxPathLength; {
		end := strings.IndexByte(escapedPath[start:], '/')
		if end < 0 {
			end = len(escapedPath)
		} else {
			end += start
		}
		segment := escapedPath[start:end]
		if normalized := normalizeSegment(segment); normalized != segment {
			if b.Cap() == 0 {
				b.Grow(min(len(escapedPath), maxPathLength))
			}
			appendPath(&b, escapedPath[copied:start])
			appendPath(&b, normalized)
			copied = end
		}
		start = end + 1
	}
	if b.Cap() == 0 {
		return escapedPath[:min(len(escapedPath), maxPathLength)]
	}
	appendPath(&b, escapedPath[copied:])
	return b.String()
}

func appendPath(b *strings.Builder, s string) {
	b.WriteString(s[:min(len(s), maxPathLength-b.Len())])
}

func normalizeSegment(escaped string) string {
	s := escaped
	if strings.IndexByte(s, '%') >= 0 {
		if decoded, err := url.PathUnescape(s); err == nil {
			s = decoded
		}
	}
	if s == "" {
		return escaped
	}
	// Telegram credentials must be removed before any length truncation,
	// including short tokens and escaped colons/digits.
	if strings.HasPrefix(s, "bot") {
		i := 3
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i > 3 && i < len(s) && s[i] == ':' {
			return "bot:token"
		}
	}
	if isUUID(s) {
		return ":id"
	}
	digits, hex, symbol := true, true, len(s) <= 15 && (s[0] >= 'A' && s[0] <= 'Z')
	// Yahoo index tickers such as ^GSPC also start with a caret.
	if len(s) >= 2 && len(s) <= 15 && s[0] == '^' && s[1] >= 'A' && s[1] <= 'Z' {
		symbol = true
	}
	hasLetter, hasDigit := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		digit := c >= '0' && c <= '9'
		upper := c >= 'A' && c <= 'Z'
		letter := upper || c >= 'a' && c <= 'z'
		digits = digits && digit
		hex = hex && (digit || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F')
		symbol = symbol && (upper || digit || c == '.' || c == '-' || c == '^' || c == '=')
		hasLetter = hasLetter || letter
		hasDigit = hasDigit || digit
	}
	switch {
	case digits:
		return ":n"
	case len(s) >= 16 && hex:
		return ":id"
	// Pure-letter segments are static endpoint names (e.g. camelCase
	// globalLongShortAccountRatio) and stay readable; only digit-bearing or very
	// long opaque segments are treated as tokens.
	case len(s) >= 40 || len(s) >= 16 && hasLetter && hasDigit:
		return ":token"
	case symbol:
		return ":sym"
	default:
		return escaped
	}
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if s[i] != '-' {
				return false
			}
			continue
		}
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

type pathKey struct {
	host string
	path string
}

type pathMeter struct {
	entries sync.Map     // pathKey -> *hostStats; never deleted
	count   atomic.Int64 // reserved ordinary keys; overflow buckets are excluded
}

func (m *pathMeter) forPath(host, path string) *hostStats {
	key := pathKey{host, path}
	if v, ok := m.entries.Load(key); ok {
		return v.(*hostStats)
	}
	if path != otherPath {
		// Only misses contend for a slot. Reserving before LoadOrStore keeps
		// the hard limit even when many different keys arrive concurrently.
		for n := m.count.Load(); n < maxPaths; n = m.count.Load() {
			if !m.count.CompareAndSwap(n, n+1) {
				continue
			}
			v, loaded := m.entries.LoadOrStore(key, &hostStats{})
			if loaded {
				m.count.Add(-1)
			}
			return v.(*hostStats)
		}
		// A concurrent insertion may have filled the last slot after our
		// first Load. Preserve attribution for that existing key.
		if v, ok := m.entries.Load(key); ok {
			return v.(*hostStats)
		}
		key.path = otherPath
	}
	if v, ok := m.entries.Load(key); ok {
		return v.(*hostStats)
	}
	v, _ := m.entries.LoadOrStore(key, &hostStats{})
	return v.(*hostStats)
}

// PathUsage is cumulative HTTP usage for a host and normalized path.
type PathUsage struct {
	Host     string `json:"host"`
	Path     string `json:"path"`
	Requests int64  `json:"requests"`
	Up       int64  `json:"up"`
	Down     int64  `json:"down"`
}

// SnapshotPaths returns at most limit paths, ordered by total bytes descending
// and then lexically by host+path. A nonpositive limit returns an empty slice.
func SnapshotPaths(limit int) []PathUsage {
	return paths.snapshot(limit)
}

func (m *pathMeter) snapshot(limit int) []PathUsage {
	list := make([]PathUsage, 0)
	if limit <= 0 {
		return list
	}
	m.entries.Range(func(key, value any) bool {
		k, stats := key.(pathKey), value.(*hostStats)
		list = append(list, PathUsage{
			Host: k.host, Path: k.path,
			Requests: stats.requests.Load(), Up: stats.up.Load(), Down: stats.down.Load(),
		})
		return true
	})
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.Up+a.Down != b.Up+b.Down {
			return a.Up+a.Down > b.Up+b.Down
		}
		return a.Host+a.Path < b.Host+b.Path
	})
	return list[:min(limit, len(list))]
}
