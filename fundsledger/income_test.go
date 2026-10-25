package fundsledger

import "testing"

func TestClassifyIncome(t *testing.T) {
	for category, types := range map[string][]string{"transfer": {"TRANSFER", "INTERNAL_TRANSFER", "CROSS_COLLATERAL_TRANSFER", "FUTURE_NEW_TRANSFER_TYPE", "DEPOSIT", "WITHDRAW"}, "trading": {"REALIZED_PNL"}, "fee": {"COMMISSION"}, "funding": {"FUNDING_FEE"}, "other": {"COMMISSION_REBATE", "API_REBATE", "REFERRAL_KICKBACK", "WELCOME_BONUS", "CONTEST_REWARD", "SOMETHING_NEW", ""}} {
		for _, typ := range types {
			if got := ClassifyIncome(typ); got != category {
				t.Errorf("%s = %s want %s", typ, got, category)
			}
		}
	}
}
