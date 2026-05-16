package strategy

import (
	"math"
	"testing"
	"time"
)

func TestEMA(t *testing.T) {
	got := EMA([]float64{1, 2, 3}, 2)
	if len(got) != 3 {
		t.Fatalf("expected 3 values, got %d", len(got))
	}
	if got[0] != 0 {
		t.Fatalf("first EMA should be zero before SMA seed, got %f", got[0])
	}
	if math.Abs(got[1]-1.5) > 1e-9 {
		t.Fatalf("EMA should seed from SMA, got %f", got[1])
	}
	if got[2] <= got[1] {
		t.Fatalf("EMA should rise for rising input: %+v", got)
	}
}

func TestATR(t *testing.T) {
	now := time.Now()
	candles := []Candle{
		{OpenTime: now, High: 11, Low: 9, Close: 10},
		{OpenTime: now.Add(time.Hour), High: 13, Low: 10, Close: 12},
		{OpenTime: now.Add(2 * time.Hour), High: 14, Low: 11, Close: 13},
	}
	got := ATR(candles, 2)
	if len(got) != len(candles) {
		t.Fatalf("expected ATR length %d, got %d", len(candles), len(got))
	}
	if got[0] != 0 || got[1] != 0 {
		t.Fatalf("ATR should be zero before warmup, got %+v", got)
	}
	if got[2] <= 0 {
		t.Fatalf("expected positive ATR, got %f", got[2])
	}
}

func TestLiquidationPriceUsesMaintenanceBracket(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaintenanceBrackets = []MaintenanceBracket{{
		NotionalFloor:    0,
		NotionalCap:      50000,
		MaintMarginRatio: 0.004,
		Cum:              0,
		InitialLeverage:  50,
	}}
	liq := EstimatedLiquidationPrice(100, Long, 2, 1000, 1000, cfg)
	if liq <= 50 || liq >= 51 {
		t.Fatalf("expected long liquidation to include MMR near 50.2, got %f", liq)
	}
	if !LiquidationGuard(100, Long, 70, 2, 1000, 1000, cfg) {
		t.Fatal("expected stop to pass liquidation guard")
	}
	if LiquidationGuard(100, Long, 60, 2, 1000, 1000, cfg) {
		t.Fatal("expected stop too close to liquidation distance to fail")
	}
}

func TestCrossMarginUsesAccountEquity(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MarginMode = MarginCross
	cfg.MaintenanceBrackets = []MaintenanceBracket{{
		NotionalFloor:    0,
		NotionalCap:      50000,
		MaintMarginRatio: 0.004,
		Cum:              0,
		InitialLeverage:  50,
	}}
	isolated := DefaultConfig()
	isolated.MaintenanceBrackets = cfg.MaintenanceBrackets
	liqIsolated := EstimatedLiquidationPrice(100, Long, 2, 1000, 5000, isolated)
	liqCross := EstimatedLiquidationPrice(100, Long, 2, 1000, 5000, cfg)
	if liqCross >= liqIsolated {
		t.Fatalf("expected cross liquidation %f to be below isolated %f", liqCross, liqIsolated)
	}
}
