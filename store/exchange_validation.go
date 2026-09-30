package store

import "fmt"

func ValidateExchangeEnvironment(exchangeType string, testnet bool) error {
	// Reject unsupported sandbox configurations rather than silently trading live.
	if testnet && exchangeType != "hyperliquid" && exchangeType != "lighter" {
		return fmt.Errorf("testnet is not supported for %s", exchangeType)
	}
	return nil
}
