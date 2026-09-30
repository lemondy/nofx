package security

import (
	"fmt"
	"io"
)

const MaxResponseBytes = 8 << 20

// Bound memory even when an upstream returns a chunked or incorrect response.
func ReadResponseBody(reader io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, MaxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > MaxResponseBytes {
		return nil, fmt.Errorf("upstream response exceeds %d bytes", MaxResponseBytes)
	}
	return body, nil
}
