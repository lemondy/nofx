package api

import (
	"context"
	"sync"
	"time"
)

const (
	// uiReadSoftWait caps how long a UI request waits on a live exchange read
	// when a recent cached snapshot is already available. If the exchange call
	// stalls (e.g. proxy timeout), the UI gets the cached snapshot within 4s
	// while the singleflight fetch finishes in the background.
	uiReadSoftWait = 4 * time.Second
	// uiReadStaleTTL bounds how old a cached account/positions snapshot may be
	// when serving a stale fallback during an exchange/proxy outage.
	uiReadStaleTTL = 10 * time.Minute
	// uiRiskStaleTTL is tighter for risk-status so breaker flags stay current.
	uiRiskStaleTTL = 2 * time.Minute
)

type uiReadEntry[T any] struct {
	value T
	at    time.Time
}

type uiReadCall[T any] struct {
	done chan struct{}
	val  T
	err  error
}

// uiReadCache coalesces concurrent dashboard reads per trader and degrades
// gracefully to the last known good snapshot when the live exchange call
// either fails or exceeds softWait. Healthy requests always return fresh live
// data (freshTTL = 0). The zero value is ready to use.
type uiReadCache[T any] struct {
	mu       sync.Mutex
	entries  map[string]uiReadEntry[T]
	inFlight map[string]*uiReadCall[T]
	now      func() time.Time
}

func (c *uiReadCache[T]) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *uiReadCache[T]) lookupStale(key string, staleTTL time.Duration) (T, time.Time, bool) {
	var zero T
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || e.at.IsZero() {
		return zero, time.Time{}, false
	}
	if staleTTL > 0 && c.clock().Sub(e.at) > staleTTL {
		return zero, time.Time{}, false
	}
	return e.value, e.at, true
}

// get runs fetch (coalesced per key via singleflight). If a valid cached
// snapshot exists within staleTTL and the live call either fails or exceeds
// softWait, get returns (cachedValue, true, cachedAt, nil) while letting the
// in-flight fetch finish and refresh the cache in the background.
func (c *uiReadCache[T]) get(
	ctx context.Context,
	key string,
	softWait time.Duration,
	staleTTL time.Duration,
	shouldCache func(T) bool,
	fetch func() (T, error),
) (T, bool, time.Time, error) {
	c.mu.Lock()
	if c.entries == nil {
		c.entries = map[string]uiReadEntry[T]{}
	}
	if c.inFlight == nil {
		c.inFlight = map[string]*uiReadCall[T]{}
	}
	call := c.inFlight[key]
	if call == nil {
		call = &uiReadCall[T]{done: make(chan struct{})}
		c.inFlight[key] = call
		go func(k string, fl *uiReadCall[T]) {
			val, err := fetch()
			fl.val, fl.err = val, err
			c.mu.Lock()
			if err == nil && (shouldCache == nil || shouldCache(val)) {
				if len(c.entries) > 1000 {
					c.entries = map[string]uiReadEntry[T]{}
				}
				c.entries[k] = uiReadEntry[T]{value: val, at: c.clock()}
			}
			delete(c.inFlight, k)
			c.mu.Unlock()
			close(fl.done)
		}(key, call)
	}
	c.mu.Unlock()

	staleVal, staleAt, hasStale := c.lookupStale(key, staleTTL)

	if hasStale && softWait > 0 {
		timer := time.NewTimer(softWait)
		defer timer.Stop()
		select {
		case <-call.done:
			if call.err == nil {
				return call.val, false, time.Time{}, nil
			}
			return staleVal, true, staleAt, nil
		case <-timer.C:
			return staleVal, true, staleAt, nil
		case <-ctx.Done():
			return staleVal, true, staleAt, nil
		}
	}

	select {
	case <-call.done:
		if call.err == nil {
			return call.val, false, time.Time{}, nil
		}
		if hasStale {
			return staleVal, true, staleAt, nil
		}
		var zero T
		return zero, false, time.Time{}, call.err
	case <-ctx.Done():
		if hasStale {
			return staleVal, true, staleAt, nil
		}
		var zero T
		return zero, false, time.Time{}, ctx.Err()
	}
}
