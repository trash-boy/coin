package m15v2

import (
	"testing"
	"time"
)

func TestWhitelistAllowed(t *testing.T) {
	wl := NewWhitelist(0.30, 3)
	now := time.Now()
	// 5 trades, mostly winners -> high sharpe
	wins := []TradeStat{
		{Win: true, PnL: 100, ClosedAt: now},
		{Win: true, PnL: 80, ClosedAt: now},
		{Win: true, PnL: 90, ClosedAt: now},
		{Win: false, PnL: -30, ClosedAt: now},
		{Win: true, PnL: 110, ClosedAt: now},
	}
	wl.Update("BTC", wins, now)
	if !wl.Allowed("BTC") {
		t.Fatal("expected BTC allowed with strong winners")
	}

	// losers
	losers := []TradeStat{
		{Win: false, PnL: -50, ClosedAt: now},
		{Win: false, PnL: -40, ClosedAt: now},
		{Win: false, PnL: -60, ClosedAt: now},
	}
	wl.Update("ETH", losers, now)
	if wl.Allowed("ETH") {
		t.Fatal("losers should not be allowed")
	}

	// too few trades
	wl.Update("SOL", wins[:2], now)
	if wl.Allowed("SOL") {
		t.Fatal("min-trades guard should block")
	}
}

func TestWhitelistFallback(t *testing.T) {
	wl := NewWhitelist(99.0, 3) // impossibly high so nothing passes
	now := time.Now()
	for _, s := range []string{"A", "B", "C"} {
		wl.Update(s, []TradeStat{
			{PnL: 10, ClosedAt: now},
			{PnL: 20, ClosedAt: now},
			{PnL: -5, ClosedAt: now},
		}, now)
	}
	out := wl.AllowedOrFallback([]string{"A", "B", "C"}, 2)
	if len(out) == 0 {
		t.Fatal("fallback should pick top-K by sharpe")
	}
}
