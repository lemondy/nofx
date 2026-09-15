package trader

import (
	"strings"
	"testing"
	"time"

	"nofx/store"
)

func mkClosed(symbol string, exitHoursAgo float64, pnl float64, nowMs int64) store.TraderPosition {
	return store.TraderPosition{
		Symbol:      symbol,
		Status:      "CLOSED",
		RealizedPnL: pnl,
		ExitTime:    nowMs - int64(exitHoursAgo*3600*1000),
	}
}

func TestLossStreakVerdict(t *testing.T) {
	// positions must be newest-first (the store orders exit_time DESC).
	now := time.Date(2026, 9, 6, 22, 0, 0, 0, time.UTC)
	nowMs := now.UnixMilli()

	t.Run("NEAR prototype: 3 rapid losses within 24h blocked", func(t *testing.T) {
		positions := []store.TraderPosition{
			mkClosed("NEARUSDT", 2, -0.97, nowMs),
			mkClosed("NEARUSDT", 4, -0.56, nowMs),
			mkClosed("NEARUSDT", 6, -0.09, nowMs),
		}
		blocked, reason := lossStreakVerdict(positions, 3, nowMs)
		if !blocked {
			t.Fatal("3 consecutive losses must block")
		}
		if reason == "" {
			t.Fatal("expected a reason with the ban deadline")
		}
	})

	t.Run("a profitable trade breaks the streak", func(t *testing.T) {
		positions := []store.TraderPosition{
			mkClosed("NEARUSDT", 2, -0.97, nowMs),
			mkClosed("NEARUSDT", 4, -0.56, nowMs),
			mkClosed("NEARUSDT", 6, +1.34, nowMs), // win breaks the run
			mkClosed("NEARUSDT", 8, -0.09, nowMs),
		}
		if blocked, _ := lossStreakVerdict(positions, 3, nowMs); blocked {
			t.Fatal("streak interrupted by a win must not block")
		}
	})

	t.Run("losses outside the 24h window do not count", func(t *testing.T) {
		positions := []store.TraderPosition{
			mkClosed("NEARUSDT", 2, -0.97, nowMs),
			mkClosed("NEARUSDT", 5, -0.56, nowMs),
			mkClosed("NEARUSDT", 30, -0.09, nowMs), // outside the window
		}
		if blocked, _ := lossStreakVerdict(positions, 3, nowMs); blocked {
			t.Fatal("out-of-window losses must be ignored")
		}
	})

	t.Run("ban expires 24h after the triggering (3rd) loss", func(t *testing.T) {
		// 3 losses: 6h, 5h, 4h ago — the 3rd (triggering) loss closed 6h ago,
		// so the ban runs until now+18h.
		positions := []store.TraderPosition{
			mkClosed("NEARUSDT", 4, -1, nowMs),
			mkClosed("NEARUSDT", 5, -1, nowMs),
			mkClosed("NEARUSDT", 6, -1, nowMs),
		}
		blocked, reason := lossStreakVerdict(positions, 3, nowMs)
		if !blocked {
			t.Fatal("ban active while 24h from the 3rd loss has not elapsed")
		}
		if !strings.Contains(reason, "18.0h left") {
			t.Fatalf("reason should mention ~18h remaining: %s", reason)
		}
	})

	t.Run("ban expired: same streak no longer blocks", func(t *testing.T) {
		// 3 losses all closed >24h ago → outside the window entirely.
		positions := []store.TraderPosition{
			mkClosed("NEARUSDT", 25, -1, nowMs),
			mkClosed("NEARUSDT", 26, -1, nowMs),
			mkClosed("NEARUSDT", 27, -1, nowMs),
		}
		if blocked, _ := lossStreakVerdict(positions, 3, nowMs); blocked {
			t.Fatal("streak fully outside the window must not block")
		}
	})

	t.Run("breakeven is not a loss", func(t *testing.T) {
		positions := []store.TraderPosition{
			mkClosed("NEARUSDT", 2, 0, nowMs),
			mkClosed("NEARUSDT", 4, -1, nowMs),
			mkClosed("NEARUSDT", 6, -1, nowMs),
		}
		if blocked, _ := lossStreakVerdict(positions, 3, nowMs); blocked {
			t.Fatal("a 0-PnL trade breaks the losing streak")
		}
	})

	t.Run("more losses during the ban do not refresh it", func(t *testing.T) {
		// Streak of 5: trigger fired at the 3rd most recent loss (7h ago).
		// Ban ends 17h from now even though losses #4/#5 are 6h/5h old.
		positions := []store.TraderPosition{
			mkClosed("NEARUSDT", 5, -1, nowMs),
			mkClosed("NEARUSDT", 6, -1, nowMs),
			mkClosed("NEARUSDT", 7, -1, nowMs),
			mkClosed("NEARUSDT", 8, -1, nowMs),
			mkClosed("NEARUSDT", 9, -1, nowMs),
		}
		blocked, _ := lossStreakVerdict(positions, 3, nowMs)
		if !blocked {
			t.Fatal("streak of 5 must still be banned")
		}
		// 7h-ago trigger + 24h ban → expiry at now+17h.
		expiry := now.Add(17 * time.Hour).UnixMilli()
		trigger := positions[2].ExitTime
		if trigger+int64(lossStreakBanFor/time.Millisecond) != expiry {
			t.Fatalf("ban must anchor at the 3rd loss: got expiry %d want %d",
				trigger+int64(lossStreakBanFor/time.Millisecond), expiry)
		}
	})

	t.Run("disabled or insufficient streak passes", func(t *testing.T) {
		positions := []store.TraderPosition{
			mkClosed("NEARUSDT", 2, -1, nowMs),
			mkClosed("NEARUSDT", 4, -1, nowMs),
		}
		if blocked, _ := lossStreakVerdict(positions, 3, nowMs); blocked {
			t.Fatal("2 losses < 3 must not block")
		}
		if blocked, _ := lossStreakVerdict(positions, 0, nowMs); blocked {
			t.Fatal("maxLosses=0 must disable the gate")
		}
	})
}

// lossStreakState now also feeds the prompt-side ban map
// (AutoTrader.lossStreakBannedMap → kernel.Context.LossStreakBanned): pin the
// expiry arithmetic — the ban runs 24h from the Nth (triggering) loss.
func TestLossStreakStateBannedUntil(t *testing.T) {
	nowMs := time.Now().UnixMilli()
	// Newest first: 1h ago, 3h ago, 5h ago (all losses).
	positions := []store.TraderPosition{
		mkClosed("ZECUSDT", 1, -1.0, nowMs),
		mkClosed("ZECUSDT", 3, -0.5, nowMs),
		mkClosed("ZECUSDT", 5, -0.8, nowMs),
	}
	streak, until := lossStreakState(positions, 3, nowMs)
	if streak != 3 {
		t.Fatalf("streak = %d, want 3", streak)
	}
	want := time.UnixMilli(positions[2].ExitTime).Add(24 * time.Hour)
	if !until.Equal(want) {
		t.Errorf("bannedUntil = %v, want Nth-loss exit + 24h = %v", until, want)
	}
	if _, until := lossStreakState(positions[:2], 3, nowMs); !until.IsZero() {
		t.Errorf("2 losses must not ban, got until %v", until)
	}
	// Newest trade profitable → streak broken, no ban.
	positions[0].RealizedPnL = 1.2
	if _, until := lossStreakState(positions, 3, nowMs); !until.IsZero() {
		t.Errorf("win at the tail must break the streak, got until %v", until)
	}
}
