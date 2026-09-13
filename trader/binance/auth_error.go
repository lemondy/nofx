package binance

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"nofx/logger"

	common "github.com/adshao/go-binance/v2/common"
)

// ErrAuthOrIP marks Binance auth failures (-2015/-2014): invalid API key,
// unwhitelisted IP, or missing permissions. Retrying without fixing the
// configuration is pointless — the operator must act.
var ErrAuthOrIP = errors.New("binance auth/IP error (-2015/-2014)")

// IsAuthOrIPError reports whether err is a Binance -2015/-2014 auth error
// (directly or wrapped).
func IsAuthOrIPError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrAuthOrIP) {
		return true
	}
	var apiErr *common.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code == -2015 || apiErr.Code == -2014
	}
	return false
}

// WrapAuthError checks err and, when it is an auth/IP rejection, logs a loud
// diagnostic (current egress IP included) and wraps it with ErrAuthOrIP so
// callers can detect it with errors.Is. Non-auth errors pass through
// untouched.
func WrapAuthError(source string, err error) error {
	if err == nil || !IsAuthOrIPError(err) {
		return err
	}
	LogAuthError(source, err)
	return fmt.Errorf("%w: %v", ErrAuthOrIP, err)
}

// LogAuthError logs the rejection reason together with the current egress IP
// so the operator knows exactly which IP to whitelist.
func LogAuthError(source string, err error) {
	logger.Errorf("🚨 [Auth] %s: Binance rejected the request (%v)", source, err)
	logger.Errorf("🚨 [Auth] This is an API key / IP whitelist / permission problem — retrying will NOT help. Trading is paused for this trader.")
	logger.Errorf("🚨 [Auth] Current egress IP: %s — add it to the Binance API key whitelist, then restart the trader.", GetCurrentPublicIP())
}

// GetCurrentPublicIP does a best-effort egress IP lookup (5s timeout per
// service, falls through several providers).
func GetCurrentPublicIP() string {
	services := []string{
		"https://api4.ipify.org?format=text",
		"https://ipv4.icanhazip.com",
		"https://api.ipify.org?format=text",
	}
	client := &http.Client{Timeout: 5 * time.Second}
	for _, s := range services {
		resp, err := client.Get(s)
		if err != nil {
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 128))
		resp.Body.Close()
		if readErr != nil || resp.StatusCode != http.StatusOK {
			continue
		}
		ip := strings.TrimSpace(string(body))
		if ip != "" {
			return ip
		}
	}
	return "unknown"
}
