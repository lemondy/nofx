package store

import (
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRegistrationAllowsExactlyOneUser(t *testing.T) {
	st := newTestStore(t)
	var wg sync.WaitGroup
	var successes atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if st.User().CreateFirst(&User{ID: fmt.Sprint(i), Email: fmt.Sprintf("%d@test.com", i)}) == nil {
				successes.Add(1)
			}
		}(i)
	}
	wg.Wait()
	count, err := st.User().Count()
	if err != nil || count != 1 || successes.Load() != 1 {
		t.Fatalf("count=%d successes=%d error=%v", count, successes.Load(), err)
	}
}

func TestActivationMissingOrDefaultDoesNotDeactivate(t *testing.T) {
	st := newTestStore(t)
	for _, row := range []*Strategy{{ID: "owned", UserID: "u", IsActive: true}, {ID: "global", UserID: "default", IsDefault: true}} {
		if err := st.Strategy().Create(row); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"missing", "global"} {
		if err := st.Strategy().SetActive("u", id); err == nil {
			t.Fatal("invalid activation succeeded")
		}
	}
	var got Strategy
	if err := st.gdb.First(&got, "id = ?", "owned").Error; err != nil || !got.IsActive {
		t.Fatalf("active strategy lost: %+v %v", got, err)
	}
}

func TestOrdersScopedIdempotentAndLegacyIndexMigration(t *testing.T) {
	st := newTestStore(t)
	if err := st.gdb.Migrator().DropIndex(&TraderOrder{}, "idx_orders_exchange_unique"); err != nil {
		t.Fatal(err)
	}
	if err := st.gdb.Exec("CREATE UNIQUE INDEX idx_orders_exchange_unique ON trader_orders(exchange_order_id)").Error; err != nil {
		t.Fatal(err)
	}
	if err := st.Order().InitTables(); err != nil {
		t.Fatal(err)
	}
	for _, exchange := range []string{"a", "b"} {
		one := &TraderOrder{TraderID: "t", ExchangeID: exchange, ExchangeOrderID: "123", Symbol: "BTCUSDT"}
		if err := st.Order().CreateOrder(one); err != nil {
			t.Fatal(err)
		}
		two := &TraderOrder{TraderID: "t", ExchangeID: exchange, ExchangeOrderID: "123", Symbol: "BTCUSDT"}
		if err := st.Order().CreateOrder(two); err != nil || two.ID != one.ID {
			t.Fatalf("duplicate IDs %d %d err=%v", one.ID, two.ID, err)
		}
	}
	var count int64
	st.gdb.Model(&TraderOrder{}).Count(&count)
	if count != 2 {
		t.Fatalf("count %d", count)
	}
}

func TestTraderDeleteCascadesAndRejectsForeignOwner(t *testing.T) {
	st := newTestStore(t)
	for _, row := range []*Trader{{ID: "a", UserID: "u"}, {ID: "b", UserID: "v"}} {
		if err := st.Trader().Create(row); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"a", "b"} {
		for _, row := range []interface{}{&EquitySnapshot{TraderID: id}, &DecisionRecordDB{TraderID: id}, &PendingEntryDB{TraderID: id, Symbol: "BTCUSDT", Side: "long"}, &GridConfigModel{ID: "cfg-" + id, TraderID: id}, &GridInstanceModel{ID: "inst-" + id, ConfigID: "cfg-" + id}, &GridLevelModel{ID: "level-" + id, InstanceID: "inst-" + id}} {
			if err := st.gdb.Create(row).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := st.Trader().Delete("u", "b"); err == nil {
		t.Fatal("foreign deletion accepted")
	}
	if err := st.Trader().Delete("u", "a"); err != nil {
		t.Fatal(err)
	}
	for _, model := range []interface{}{&EquitySnapshot{}, &DecisionRecordDB{}, &PendingEntryDB{}, &GridConfigModel{}, &GridInstanceModel{}, &GridLevelModel{}} {
		var count int64
		if err := st.gdb.Model(model).Count(&count).Error; err != nil || count != 1 {
			t.Fatalf("%T count=%d error=%v", model, count, err)
		}
	}
}

func TestTelegramCodeOneTimeExpiryAndUnbind(t *testing.T) {
	st := newTestStore(t)
	tg := st.TelegramConfig()
	if err := tg.Save("test-bot-token", ""); err != nil {
		t.Fatal(err)
	}
	if err := tg.BindWithCode("", 1, "attacker"); err == nil {
		t.Fatal("empty code accepted")
	}
	code, err := tg.IssueBindCode()
	if err != nil {
		t.Fatal(err)
	}
	if err := tg.BindWithCode("wrong", 1, "attacker"); err == nil {
		t.Fatal("wrong code accepted")
	}
	if err := tg.BindWithCode(code, 2, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := tg.BindWithCode(code, 1, "attacker"); err == nil {
		t.Fatal("replay accepted")
	}
	if err := tg.Unbind(); err != nil {
		t.Fatal(err)
	}
	if err := tg.BindWithCode(code, 1, "attacker"); err == nil {
		t.Fatal("old code after unbind accepted")
	}
	code, err = tg.IssueBindCode()
	if err != nil {
		t.Fatal(err)
	}
	st.gdb.Model(&TelegramConfig{}).Where("id = 1").Update("bind_code_expires", time.Now().Add(-time.Minute))
	if err := tg.BindWithCode(code, 1, "attacker"); err == nil {
		t.Fatal("expired code accepted")
	}
}

func TestUnsafeStrategyAndExchangeEnvironment(t *testing.T) {
	cfg := GetDefaultStrategyConfig("zh")
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.RiskControl.MaxMarginUsage = math.Inf(1)
	if cfg.Validate() == nil {
		t.Fatal("infinite margin accepted")
	}
	grid := &GridStrategyConfig{Symbol: "BTCUSDT", GridCount: 10, TotalInvestment: 100, Leverage: 2, LowerPrice: 100, UpperPrice: 200}
	if err := grid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, count := range []int{-1, 0, 1, 51} {
		grid.GridCount = count
		if grid.Validate() == nil {
			t.Fatalf("accepted grid count %d", count)
		}
	}
	if ValidateExchangeEnvironment("binance", true) == nil {
		t.Fatal("testnet silently accepted")
	}
	for _, typ := range []string{"hyperliquid", "lighter"} {
		if err := ValidateExchangeEnvironment(typ, true); err != nil {
			t.Fatal(err)
		}
	}
}

func TestModelKeyPreservedUntilExplicitClear(t *testing.T) {
	st := newTestStore(t)
	if err := st.gdb.Create(&AIModel{ID: "model-u", UserID: "u", Provider: "openai"}).Error; err != nil {
		t.Fatal(err)
	}
	// Legacy plaintext fixture exercises preservation without configuring real keys.
	if err := st.gdb.Exec("UPDATE ai_models SET api_key = ? WHERE id = ?", "test-credential", "model-u").Error; err != nil {
		t.Fatal(err)
	}
	if err := st.AIModel().Update("u", "model-u", false, "", "", ""); err != nil {
		t.Fatal(err)
	}
	model, err := st.AIModel().Get("u", "model-u")
	if err != nil || model.APIKey != "test-credential" {
		t.Fatal("blank edit erased key", err)
	}
	if err := st.AIModel().Update("u", "model-u", false, "", "", "", true); err != nil {
		t.Fatal(err)
	}
	model, err = st.AIModel().Get("u", "model-u")
	if err != nil || model.APIKey != "" {
		t.Fatal("explicit removal retained key", err)
	}
}

func TestStrategyCompareAndSwapRejectsInterveningWrite(t *testing.T) {
	st := newTestStore(t)
	row := &Strategy{ID: "s", UserID: "u", Config: "{}"}
	if err := st.Strategy().Create(row); err != nil {
		t.Fatal(err)
	}
	original := row.UpdatedAt
	if err := st.Strategy().Update(&Strategy{ID: "s", UserID: "u", Name: "new", Config: "{}"}, original); err != nil {
		t.Fatal(err)
	}
	if err := st.Strategy().Update(&Strategy{ID: "s", UserID: "u", Name: "stale", Config: "{}"}, original); err != ErrStrategyConflict {
		t.Fatalf("stale update: %v", err)
	}
	got, err := st.Strategy().Get("u", "s")
	if err != nil || got.Name != "new" {
		t.Fatal("intervening update lost", err)
	}
}
