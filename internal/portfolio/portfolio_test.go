package portfolio

import (
	"testing"
	"time"

	"coin/internal/strategy"
)

func TestRunEqualWeightUsesAllSymbols(t *testing.T) {
	cfg := smallConfig()
	data := map[string]SymbolData{
		"ethusdt": {Candles: syntheticCandles(60, 100), Config: cfg},
		"btcusdt": {Candles: syntheticCandles(60, 200), Config: cfg},
	}
	result, err := RunEqualWeight(data, 10000, cfg)
	if err != nil {
		t.Fatalf("RunEqualWeight returned error: %v", err)
	}
	if len(result.Results) != 2 {
		t.Fatalf("expected two symbol results, got %d", len(result.Results))
	}
	if result.InitialEquity != 10000 || result.FinalEquity <= 0 {
		t.Fatalf("unexpected portfolio equity: %+v", result)
	}
	if result.Results[0].Symbol != "BTCUSDT" || result.Results[1].Symbol != "ETHUSDT" {
		t.Fatalf("expected sorted upper-case symbols, got %+v", result.Results)
	}
	if result.Results[0].Allocation != 5000 || result.Results[1].Allocation != 5000 {
		t.Fatalf("unexpected allocation: %+v", result.Results)
	}
}

func smallConfig() strategy.Config {
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
	cfg.PullbackBars = 3
	cfg.SwingLookback = 3
	cfg.MinATRRatio = 0
	cfg.MaxATRRatio = 1
	return cfg
}

func syntheticCandles(n int, start float64) []strategy.Candle {
	now := time.Unix(1735689600, 0).UTC()
	out := make([]strategy.Candle, 0, n)
	price := start
	for i := 0; i < n; i++ {
		price += 0.1
		out = append(out, strategy.Candle{
			OpenTime:  now.Add(time.Duration(i) * time.Hour),
			CloseTime: now.Add(time.Duration(i+1) * time.Hour),
			Open:      price - 0.05,
			High:      price + 1,
			Low:       price - 1,
			Close:     price,
			Volume:    100,
		})
	}
	return out
}
