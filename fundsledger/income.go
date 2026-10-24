package fundsledger

import "strings"

// ClassifyIncome never rewrites the original income type.
func ClassifyIncome(incomeType string) string {
	t := strings.ToUpper(strings.TrimSpace(incomeType))
	switch {
	case strings.Contains(t, "TRANSFER"), t == "DEPOSIT", t == "WITHDRAW":
		return "transfer"
	case t == "REALIZED_PNL":
		return "trading"
	case t == "COMMISSION":
		return "fee"
	case t == "FUNDING_FEE":
		return "funding"
	default:
		return "other"
	}
}

func StableAsset(asset string) bool {
	switch strings.ToUpper(asset) {
	case "USDT", "USDC", "BUSD", "FDUSD", "TUSD", "DAI", "USD":
		return true
	}
	return false
}
