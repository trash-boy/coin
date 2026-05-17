package m15v2

import (
	"math"
	"time"
)

// Risk owns the dynamic sizing & guards: half-Kelly multiplier, daily loss
// limit, loss-streak cooldown.
type Risk struct {
	p Params

	trades []TradeStat // rolling, capped at KellyLookback

	dayKey       string
	dayStartEq   float64
	dayPnL       float64
	streakLosses int
	cooldownEnd  time.Time
}

func NewRisk(p Params) *Risk {
	return &Risk{p: p, trades: make([]TradeStat, 0, p.KellyLookback)}
}

// RecordTrade should be called after each fully-closed position.
func (r *Risk) RecordTrade(t TradeStat, equity float64) {
	r.trades = append(r.trades, t)
	if len(r.trades) > r.p.KellyLookback {
		r.trades = r.trades[len(r.trades)-r.p.KellyLookback:]
	}
	r.dayPnL += t.PnL
	if t.PnL < 0 {
		r.streakLosses++
		if r.streakLosses >= r.p.LossStreakHalt {
			r.cooldownEnd = t.ClosedAt.Add(time.Duration(r.p.CooldownMinutes) * time.Minute)
			r.streakLosses = 0
		}
	} else {
		r.streakLosses = 0
	}
}

// OnNewBar should be called once per bar to roll the daily counter.
func (r *Risk) OnNewBar(now time.Time, equity float64) {
	key := now.UTC().Format("2006-01-02")
	if key != r.dayKey {
		r.dayKey = key
		r.dayStartEq = equity
		r.dayPnL = 0
	}
}

// CanOpen consults all guards and reports whether a new entry is allowed.
func (r *Risk) CanOpen(now time.Time) (bool, string) {
	if now.Before(r.cooldownEnd) {
		return false, "loss-streak cooldown"
	}
	if r.dayStartEq > 0 && r.dayPnL <= -r.p.DailyLossLimit*r.dayStartEq {
		return false, "daily loss limit"
	}
	return true, ""
}

// kellyMultiplier computes a fraction-of-base scaling factor in [floor, cap].
// Uses simple win-rate / avg-R Kelly: f* = W - (1-W)/R.
func (r *Risk) kellyMultiplier() float64 {
	if !r.p.UseKelly || len(r.trades) < r.p.KellyLookback/2 {
		return 1.0
	}
	wins, losses := 0, 0
	sumWin, sumLoss := 0.0, 0.0
	for _, t := range r.trades {
		if t.PnL > 0 {
			wins++
			sumWin += t.PnL
		} else if t.PnL < 0 {
			losses++
			sumLoss += -t.PnL
		}
	}
	n := wins + losses
	if n == 0 || losses == 0 {
		return r.p.KellyFloor
	}
	W := float64(wins) / float64(n)
	avgWin := sumWin / math.Max(1, float64(wins))
	avgLoss := sumLoss / float64(losses)
	if avgLoss == 0 {
		return r.p.KellyFloor
	}
	R := avgWin / avgLoss
	f := W - (1-W)/R
	f *= r.p.KellyFraction
	if math.IsNaN(f) || f < r.p.KellyFloor {
		return r.p.KellyFloor
	}
	if f > r.p.KellyCap {
		return r.p.KellyCap
	}
	return f
}

// SizeQty computes the position size (in base-asset qty) for a candidate trade.
// equity      : current account equity (USDT)
// entry, stop : prices
// returns 0 when sizing yields no exposure.
func (r *Risk) SizeQty(equity, entry, stop float64) float64 {
	if equity <= 0 || entry <= 0 || stop <= 0 {
		return 0
	}
	risk := math.Abs(entry - stop)
	if risk <= 0 {
		return 0
	}
	mult := r.kellyMultiplier()
	dollarRisk := equity * r.p.RiskPerTrade * mult
	qty := dollarRisk / risk

	// leverage cap: notional <= equity * MaxLeverage
	maxNotional := equity * r.p.MaxLeverage
	if qty*entry > maxNotional {
		qty = maxNotional / entry
	}
	return qty
}
