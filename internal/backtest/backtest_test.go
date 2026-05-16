package backtest

import (
	"testing"
	"time"

	"coin/internal/strategy"
)

func TestRunNeedsEnoughCandles(t *testing.T) {
	cfg := strategy.DefaultConfig()
	_, err := Run(nil, 1000, cfg)
	if err == nil {
		t.Fatal("expected error for missing candles")
	}
}

func TestRunSyntheticTrend(t *testing.T) {
	cfg := strategy.DefaultConfig()
	cfg.FastEMA = 3
	cfg.SlowEMA = 5
	cfg.TrendEMA = 8
	cfg.ATRPeriod = 3
	cfg.HigherTFHours = 1
	cfg.MajorTFHours = 1
	cfg.HigherFastEMA = 3
	cfg.HigherSlowEMA = 5
	cfg.ADXPeriod = 3
	cfg.EfficiencyPeriod = 3
	cfg.MinATRRatio = 0
	cfg.MaxATRRatio = 1

	now := time.Now().UTC()
	candles := make([]strategy.Candle, 0, 80)
	price := 100.0
	for i := 0; i < 80; i++ {
		price += 0.7
		candles = append(candles, strategy.Candle{
			OpenTime:  now.Add(time.Duration(i) * time.Hour),
			CloseTime: now.Add(time.Duration(i+1) * time.Hour),
			Open:      price - 0.2,
			High:      price + 1.0,
			Low:       price - 1.0,
			Close:     price,
			Volume:    100,
		})
	}
	result, err := Run(candles, 10000, cfg)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.FinalEquity <= 0 {
		t.Fatalf("expected positive final equity, got %f", result.FinalEquity)
	}
}

func TestClosePositionFeeAccounting(t *testing.T) {
	cfg := strategy.DefaultConfig()
	cfg.FeeRate = 0.001
	pos := position{
		side:       strategy.Long,
		entryTime:  time.Unix(0, 0),
		entryPrice: 100,
		qty:        2,
		entryFee:   0.2,
	}
	balance, trades := closePosition(999.8, pos, 110, time.Unix(3600, 0), cfg, nil, "test")
	if len(trades) != 1 {
		t.Fatalf("expected one trade, got %d", len(trades))
	}
	if trades[0].EntryFee != 0.2 {
		t.Fatalf("entry fee mismatch: %f", trades[0].EntryFee)
	}
	if trades[0].ExitFee <= 0 {
		t.Fatalf("expected positive exit fee")
	}
	expectedBalance := 999.8 + (trades[0].Exit-pos.entryPrice)*pos.qty - trades[0].ExitFee
	if mathAbs(balance-expectedBalance) > 1e-9 {
		t.Fatalf("balance mismatch got %f want %f", balance, expectedBalance)
	}
	if mathAbs(trades[0].PnL-((trades[0].Exit-pos.entryPrice)*pos.qty-trades[0].EntryFee-trades[0].ExitFee)) > 1e-9 {
		t.Fatalf("trade pnl fee accounting mismatch")
	}
}

func TestGapStopUsesOpenPrice(t *testing.T) {
	cfg := strategy.DefaultConfig()
	cfg.SlippageBps = 0
	pos := position{
		side:       strategy.Long,
		entryTime:  time.Unix(0, 0),
		entryPrice: 100,
		qty:        1,
		trail:      95,
	}
	c := strategy.Candle{
		OpenTime:  time.Unix(3600, 0),
		CloseTime: time.Unix(7200, 0),
		Open:      90,
		High:      92,
		Low:       88,
		Close:     91,
	}
	_, trades, exited := maybeExitIntrabar(1000, pos, c, cfg, nil)
	if !exited || len(trades) != 1 {
		t.Fatalf("expected gap stop exit, exited=%v trades=%d", exited, len(trades))
	}
	if trades[0].Exit != 90 {
		t.Fatalf("expected gap stop at open 90, got %f", trades[0].Exit)
	}
}

func TestExecuteEntryUsesNextOpenPrice(t *testing.T) {
	cfg := strategy.DefaultConfig()
	cfg.SlippageBps = 0
	pending := &pendingOrder{
		side:      strategy.Long,
		stop:      90,
		atr:       2,
		riskScale: 1,
	}
	c := strategy.Candle{
		OpenTime:  time.Unix(3600, 0),
		CloseTime: time.Unix(7200, 0),
		Open:      110,
		High:      210,
		Low:       100,
		Close:     200,
	}
	_, pos, opened := executeEntry(10000, 10000, pending, c, cfg)
	if !opened {
		t.Fatal("expected entry to open")
	}
	if pos.entryTime != c.OpenTime {
		t.Fatalf("expected entry time to be next candle open, got %s", pos.entryTime)
	}
	if pos.entryPrice != c.Open {
		t.Fatalf("expected entry at open %f, got %f", c.Open, pos.entryPrice)
	}
}

func TestExecuteAddRaisesTrailingStop(t *testing.T) {
	cfg := strategy.DefaultConfig()
	cfg.SlippageBps = 0
	pos := position{
		side:       strategy.Long,
		entryTime:  time.Unix(0, 0),
		entryPrice: 100,
		qty:        1,
		trail:      95,
		lastAdd:    100,
	}
	pending := &pendingOrder{
		side:      strategy.Long,
		stop:      110,
		atr:       3,
		riskScale: 1,
	}
	c := strategy.Candle{
		OpenTime:  time.Unix(3600, 0),
		CloseTime: time.Unix(7200, 0),
		Open:      120,
		High:      125,
		Low:       118,
		Close:     123,
	}
	_, updated, added := executeAdd(10000, 10000, pos, pending, c, 42, cfg)
	if !added {
		t.Fatal("expected add to execute")
	}
	if updated.trail != 110 {
		t.Fatalf("expected trail to be raised to add stop, got %f", updated.trail)
	}
}

func TestCanPyramidRequiresSpacing(t *testing.T) {
	cfg := strategy.DefaultConfig()
	cfg.MinAddSpacingBars = 4
	pos := position{
		side:       strategy.Long,
		addCount:   1,
		lastAdd:    100,
		lastAddBar: 10,
	}
	if canPyramid(pos, 120, 5, 12, cfg) {
		t.Fatal("expected pyramid to be blocked by spacing")
	}
	if !canPyramid(pos, 130, 5, 14, cfg) {
		t.Fatal("expected pyramid after spacing and favorable move")
	}
}

func TestKellyScaleRequiresFullLookback(t *testing.T) {
	cfg := strategy.DefaultConfig()
	cfg.KellyLookback = 30
	trades := make([]Trade, 29)
	for i := range trades {
		trades[i] = Trade{PnL: 10}
	}
	if got := kellyScale(trades, cfg); got != 1 {
		t.Fatalf("expected Kelly disabled until full lookback, got %f", got)
	}
	trades = append(trades, Trade{PnL: -5})
	got := kellyScale(trades, cfg)
	if got < cfg.MinKellyScale || got > cfg.MaxKellyScale {
		t.Fatalf("Kelly scale out of bounds: %f", got)
	}
}

func mathAbs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
