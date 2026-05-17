package m15v2

import (
	"math"
	"math/rand"
	"testing"
	"time"
)

func TestParamsValidate(t *testing.T) {
	p := DefaultParams()
	if err := p.Validate(); err != nil {
		t.Fatalf("default params invalid: %v", err)
	}
}

func TestWarmupBars(t *testing.T) {
	p := DefaultParams()
	w := p.WarmupBars()
	if w < p.EMASlow {
		t.Fatalf("warmup must be >= EMASlow, got %d", w)
	}
}

func TestKellyFloorOnEmpty(t *testing.T) {
	p := DefaultParams()
	r := NewRisk(p)
	if got := r.kellyMultiplier(); got != 1.0 {
		t.Fatalf("expected 1.0 with no trades, got %v", got)
	}
}

func TestSizeQtyLeverageCap(t *testing.T) {
	p := DefaultParams()
	r := NewRisk(p)
	// equity 1000, entry 100, stop 99 -> 1$ risk per unit, 1.5% risk = 15$,
	// raw qty 15, notional 1500 -> capped at equity*5 = 5000 OK
	q := r.SizeQty(1000, 100, 99)
	if q <= 0 || q > 50 {
		t.Fatalf("unexpected qty %v", q)
	}
}

// Smoke test: feed a synthetic uptrend and ensure the strategy can reach the
// "ready" state and at least attempt entries without panicking.
func TestStrategySmoke(t *testing.T) {
	p := DefaultParams()
	p.SqueezePct = 0.99   // relax squeeze to make entries possible
	p.ADXMin = 0           // relax ADX
	p.UseHTF = false
	s := New(p)

	rng := rand.New(rand.NewSource(42))
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	price := 100.0
	for i := 0; i < 600; i++ {
		drift := 0.05
		noise := (rng.Float64() - 0.5) * 0.5
		price += drift + noise
		if price < 1 {
			price = 1
		}
		bar := Bar{
			Time:   now.Add(time.Duration(i) * 15 * time.Minute),
			Open:   price,
			High:   price + math.Abs(noise),
			Low:    price - math.Abs(noise),
			Close:  price,
			Volume: 1000,
		}
		_ = s.OnBar(bar, 1000)
	}
	if !s.Ind.Ready() {
		t.Fatalf("indicators never became ready")
	}
}
