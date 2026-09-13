package binance

import (
	"errors"
	"fmt"
	"testing"

	common "github.com/adshao/go-binance/v2/common"
)

func TestIsAuthOrIPError(t *testing.T) {
	ipErr := &common.APIError{Code: -2015, Message: "Invalid API-key, IP, or permissions for action"}
	keyErr := &common.APIError{Code: -2014, Message: "API-key format invalid"}
	otherErr := &common.APIError{Code: -1021, Message: "Timestamp for this request is outside of the recvWindow"}
	wrapped := fmt.Errorf("failed to get account info: %w", WrapAuthError("GetBalance", ipErr))

	if !IsAuthOrIPError(ipErr) {
		t.Error("-2015 should be detected as auth/IP error")
	}
	if !IsAuthOrIPError(keyErr) {
		t.Error("-2014 should be detected as auth/IP error")
	}
	if !IsAuthOrIPError(wrapped) {
		t.Error("wrapped -2015 should be detected via errors.As")
	}
	if !errors.Is(wrapped, ErrAuthOrIP) {
		t.Error("wrapped error should match ErrAuthOrIP sentinel")
	}
	if IsAuthOrIPError(otherErr) {
		t.Error("-1021 (timestamp) must NOT be treated as auth error")
	}
	if IsAuthOrIPError(errors.New("context deadline exceeded")) {
		t.Error("network errors must NOT be treated as auth errors")
	}
	if IsAuthOrIPError(nil) {
		t.Error("nil must not be an auth error")
	}
}
