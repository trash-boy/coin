package xsmom

import (
	"math"
	"testing"
	"time"

	"coin/internal/strategy"
)

func mkBars(sym string, start time.Time, days int, mom float64) SymbolBars {
	d := make([]time.Time, days)
	c := make([]float64, days)
	q := make([]float64, days)
	f := make([]float64, days)
	price := 100.0
	for i := 0; i < days; i++ {
		d[i] = start.AddDate(0, 0, i)
		price *= 1 + mom
		c[i] = price
		q[i] = 50e6 // 50M USDT daily quote vol — well above the 10M floor
		f[i] = 0
	}
	return SymbolBars{Symbol: sym, Days: d, Close: c, QuoteVol: q, Funding: f}
}

func TestDefaultConfigValidates(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
}

func TestPanelEligibilityAndRanking(t *testing.T) {
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := []SymbolBars{
		mkBars("AAA", start, 100, 0.02),  // strongest mom
		mkBars("BBB", start, 100, 0.01),
		mkBars("CCC", start, 100, 0.0),
		mkBars("DDD", start, 100, -0.01),
		mkBars("EEE", start, 100, -0.02), // weakest mom
		// 6 dummy symbols so eligible >= TopN+BotN+5
		mkBars("F1", start, 100, 0.005),
		mkBars("F2", start, 100, 0.003),
		mkBars("F3", start, 100, -0.003),
		mkBars("F4", start, 100, -0.005),
		mkBars("F5", start, 100, 0.001),
		mkBars("F6", start, 100, -0.001),
	}
	p, err := BuildPanel(bars)
	if err != nil {
		t.Fatalf("build panel: %v", err)
	}
	cfg := Config{
		LookbackDays:   7,
		HoldDays:       7,
		TopN:           2,
		BotN:           2,
		Leverage:       1.0,
		MinHistDays:    60,
		MinQuoteVolUSD: 10e6,
		FeeBps:         4,
		SlipBps:        8,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	i := 80
	longs, shorts := p.SelectLongShort(i, cfg)
	if len(longs) != 2 || len(shorts) != 2 {
		t.Fatalf("expected 2/2 long/short, got %d/%d", len(longs), len(shorts))
	}
	if longs[0].Symbol != "AAA" {
		t.Errorf("expected AAA top, got %s", longs[0].Symbol)
	}
	if shorts[len(shorts)-1].Symbol != "EEE" {
		t.Errorf("expected EEE bottom, got %s", shorts[len(shorts)-1].Symbol)
	}
}

func TestRunMonotonic(t *testing.T) {
	// Long-only, with one strongly trending symbol; equity must end > start.
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := []SymbolBars{
		mkBars("AAA", start, 200, 0.02),
		mkBars("BBB", start, 200, 0.001),
		mkBars("CCC", start, 200, -0.001),
		mkBars("DDD", start, 200, -0.005),
		mkBars("EEE", start, 200, -0.01),
		mkBars("F1", start, 200, 0.0005),
		mkBars("F2", start, 200, -0.0005),
		mkBars("F3", start, 200, 0.001),
		mkBars("F4", start, 200, -0.002),
		mkBars("F5", start, 200, 0.003),
		mkBars("F6", start, 200, -0.003),
	}
	p, err := BuildPanel(bars)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		LookbackDays:   7,
		HoldDays:       7,
		TopN:           2,
		BotN:           0,
		Leverage:       1.0,
		MinHistDays:    60,
		MinQuoteVolUSD: 10e6,
		FeeBps:         4,
		SlipBps:        8,
	}
	r, err := Run(p, cfg, 0, 0, 10000)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if r.FinalEquity <= r.InitialEquity {
		t.Errorf("expected profit on monotonic up-trend; final=%.2f init=%.2f",
			r.FinalEquity, r.InitialEquity)
	}
	if r.Sharpe < 1 {
		t.Errorf("expected sharpe > 1 on noiseless trend, got %.2f", r.Sharpe)
	}
	if r.NumRebalances == 0 {
		t.Error("expected at least one rebalance")
	}
	if math.IsNaN(r.AnnReturn) {
		t.Error("annualized return is NaN")
	}
}

func TestComputeOrderDeltas(t *testing.T) {
	signal := SignalSet{
		Targets: []Target{
			{Symbol: "AAA", Side: strategy.Long, WeightFrac: 0.5, Notional: 5000},
			{Symbol: "BBB", Side: strategy.Long, WeightFrac: 0.5, Notional: 5000},
		},
	}
	current := []CurrentPosition{
		{Symbol: "AAA", Side: strategy.Long, Notional: 5050},  // within 5% — no resize
		{Symbol: "CCC", Side: strategy.Long, Notional: 4000},  // dropped — flatten
		{Symbol: "DDD", Side: strategy.Short, Notional: 1000}, // dropped — flatten
	}
	deltas := ComputeOrderDeltas(signal, current)
	// Expect: flatten CCC, flatten DDD, open BBB. AAA unchanged.
	if len(deltas) != 3 {
		t.Fatalf("expected 3 deltas, got %d", len(deltas))
	}
	flatten := 0
	open := 0
	for _, d := range deltas {
		if d.Side == strategy.Flat {
			flatten++
		}
		if d.Side == strategy.Long && d.Symbol == "BBB" {
			open++
		}
	}
	if flatten != 2 {
		t.Errorf("expected 2 flatten orders, got %d", flatten)
	}
	if open != 1 {
		t.Errorf("expected 1 open order for BBB, got %d", open)
	}
}
