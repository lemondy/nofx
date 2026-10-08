package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nofx/store"
)

// review 2026-10-09 K: server-side rule verification, apply gating, staleness.

func seedKJournal(t *testing.T, srv *Server, traderID string, highNet float64) {
	t.Helper()
	now := time.Now().UTC().UnixMilli()
	yes := true
	pos := int64(0)
	add := func(lev int, net float64) {
		pos++
		e := &store.TradeJournalDB{TraderID: traderID, PositionID: pos, Symbol: "BTCUSDT", Side: "LONG",
			EntryPrice: 100, Quantity: 1, Leverage: lev, RealizedPnL: net,
			ExitTime: now - 24*3600*1000, AIManaged: &yes, ReviewStatus: "pending"}
		if err := srv.store.GormDB().Create(e).Error; err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 4; i++ {
		add(20, highNet)
	}
	for i := 0; i < 6; i++ {
		add(5, 1)
	}
}

func TestApplyRulesVerification(t *testing.T) {
	for _, tc := range []struct {
		name    string
		highNet float64
		saved   int
	}{{"supported", -3, 1}, {"contradicted", 5, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			srv, r, token, _, traderID := idorFixture(t)
			r.POST("/review/ai/apply-rules", srv.authMiddleware(), srv.handleAIApplyRules)
			r.GET("/review/rules", srv.authMiddleware(), srv.handleRulesList)
			r.POST("/review/rules/:id/reverify", srv.authMiddleware(), srv.handleRuleReverify)
			seedKJournal(t, srv, traderID, tc.highNet)

			body := `{"rules":[{"rule_type":"hard","name":"lev","condition":"{\"field\":\"leverage\",\"op\":\">\",\"value\":10}","on_violation":"block","source":"manual","verified_stats":"forged"}]}`
			do := func(method, path, b string) map[string]any {
				req := httptest.NewRequest(method, path, strings.NewReader(b))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+token)
				out := httptest.NewRecorder()
				r.ServeHTTP(out, req)
				if out.Code != http.StatusOK {
					t.Fatalf("%s %s -> %d %s", method, path, out.Code, out.Body.String())
				}
				var m map[string]any
				_ = json.Unmarshal(out.Body.Bytes(), &m)
				return m
			}
			m := do("POST", "/review/ai/apply-rules?trader_id="+traderID, body)
			if int(m["saved"].(float64)) != tc.saved {
				t.Fatalf("saved=%v want %d: %v", m["saved"], tc.saved, m)
			}
			rules, _ := srv.store.Rule().ListRules(traderID)
			if tc.saved == 0 {
				rej := m["rejected"].([]any)
				if len(rej) != 1 || rej[0].(map[string]any)["reason"] == "" {
					t.Fatalf("expected rejection with reason: %v", m)
				}
				if len(rules) != 0 {
					t.Fatal("rejected rule was persisted")
				}
				return
			}
			if len(rules) != 1 || rules[0].Source != "ai_review" || rules[0].VerifiedAt == 0 {
				t.Fatalf("saved rule wrong: %+v", rules)
			}
			var v struct {
				Status  string `json:"status"`
				Matched int    `json:"matched"`
			}
			if err := json.Unmarshal([]byte(rules[0].VerifiedStats), &v); err != nil || v.Status != "supported" || v.Matched != 4 {
				t.Fatalf("verified_stats=%q", rules[0].VerifiedStats)
			}

			// list: trigger stats + review_due, seeded straight into the DB.
			now := time.Now().UTC().UnixMilli()
			day := int64(24 * 3600 * 1000)
			db := srv.store.GormDB()
			db.Model(&store.TradingRuleDB{}).Where("id = ?", rules[0].ID).
				Updates(map[string]any{"created_at": now - 40*day, "verified_at": now - 35*day})
			for _, at := range []int64{now - 2*day, now - 5*day, now - 45*day} {
				db.Create(&store.RuleCheckLogDB{TraderID: traderID, RuleID: rules[0].ID, Action: "open_long", Symbol: "BTCUSDT", CreatedAt: at})
			}
			lm := do("GET", "/review/rules?trader_id="+traderID, "")
			rl := lm["rules"].([]any)[0].(map[string]any)
			if rl["triggers_30d"].(float64) != 2 || int64(rl["last_triggered_at"].(float64)) != now-2*day || rl["review_due"] != true {
				t.Fatalf("list annotations wrong: %v", rl)
			}
			// reverify clears review_due
			do("POST", "/review/rules/"+jsonID(rules[0].ID)+"/reverify?trader_id="+traderID, "")
			lm = do("GET", "/review/rules?trader_id="+traderID, "")
			if lm["rules"].([]any)[0].(map[string]any)["review_due"] != false {
				t.Fatalf("review_due should clear after reverify: %v", lm)
			}
		})
	}
}

func jsonID(id int64) string { b, _ := json.Marshal(id); return string(b) }

func TestParseRuleProposalsSupportingTrades(t *testing.T) {
	ps, err := parseRuleProposals(`[{"rule_type":"hard","name":"a","condition":{"field":"leverage","op":">","value":10},"supporting_trades":7}]`)
	if err != nil || len(ps) != 1 || ps[0].SupportingTrades != 7 {
		t.Fatalf("%v %+v", err, ps)
	}
}
