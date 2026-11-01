package mcp

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

type inflightKey struct {
	host    string
	keyHash [16]byte
}

type inflightEntry struct {
	slots chan struct{}
	refs  int // holders plus callers waiting to acquire
}

type inflightLimiter struct {
	mu      sync.Mutex
	entries map[inflightKey]*inflightEntry
	logOnce sync.Once
}

var llmInflightLimiter = &inflightLimiter{}

func (l *inflightLimiter) acquire(ctx context.Context, client *Client) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit := llmMaxInflight()
	if limit == 0 {
		return func() {}, nil
	}
	l.logOnce.Do(func() {
		client.Log.Infof("LLM per-key in-flight limiter enabled: max %d concurrent HTTP attempts per host/key", limit)
	})
	u, err := url.Parse(client.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid LLM base URL: %w", err)
	}
	hash := sha256.Sum256([]byte(client.APIKey))
	key := inflightKey{host: strings.ToLower(u.Host), keyHash: [16]byte(hash[:16])}
	l.mu.Lock()
	if l.entries == nil {
		l.entries = make(map[inflightKey]*inflightEntry)
	}
	entry := l.entries[key]
	if entry == nil {
		entry = &inflightEntry{slots: make(chan struct{}, limit)}
		l.entries[key] = entry
	}
	entry.refs++
	l.mu.Unlock()

	dropRef := func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		entry.refs--
		if entry.refs == 0 {
			delete(l.entries, key)
		}
	}
	select {
	case entry.slots <- struct{}{}:
		release := sync.OnceFunc(func() {
			<-entry.slots
			dropRef()
		})
		if err := ctx.Err(); err != nil {
			release()
			return nil, err
		}
		return release, nil
	case <-ctx.Done():
		dropRef()
		return nil, ctx.Err()
	}
}

// doHTTPRequest holds a slot through body reads (including SSE), releasing it
// on transport errors or body close, before the caller can sleep or fall back.
func (client *Client) doHTTPRequest(httpClient *http.Client, req *http.Request) (*http.Response, error) {
	if llmMaxInflight() == 0 {
		return httpClient.Do(req)
	}
	release, err := llmInflightLimiter.acquire(req.Context(), client)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		release()
		return resp, err
	}
	resp.Body = &inflightBody{ReadCloser: resp.Body, release: release}
	return resp, nil
}

type inflightBody struct {
	io.ReadCloser
	release func()
}

func (b *inflightBody) Close() error {
	defer b.release()
	return b.ReadCloser.Close()
}
