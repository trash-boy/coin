package m15v2

import (
	"sort"
	"sync"
	"time"
)

// Whitelist gates entry by per-symbol rolling performance. The host passes a
// snapshot of recent trade history each rebalance window (e.g. weekly) and
// asks the filter whether a symbol is currently tradable.
//
// Usage:
//
//	wl := m15v2.NewWhitelist(0.30, 3)            // sharpeMin=0.3, minTrades=3
//	wl.Update("BTCUSDT", recentTrades)           // typically every 7d
//	if wl.Allowed("BTCUSDT") { /* run OnBar */ }
//
// Recommended cadence: re-score weekly using a rolling 30-day window.
type Whitelist struct {
	mu        sync.RWMutex
	sharpeMin float64
	minTrades int
	scored    map[string]symScore
}

type symScore struct {
	sharpe    float64
	winRate   float64
	pnl       float64
	nTrades   int
	updatedAt time.Time
}

// NewWhitelist creates a filter with the given Sharpe threshold (use 0.30
// based on 2026-05 calibration) and minimum trade count for stability.
func NewWhitelist(sharpeMin float64, minTrades int) *Whitelist {
	return &Whitelist{
		sharpeMin: sharpeMin,
		minTrades: minTrades,
		scored:    make(map[string]symScore),
	}
}

// Update recomputes the score for a symbol from a slice of recent trades
// (caller decides the lookback window).
func (w *Whitelist) Update(sym string, trades []TradeStat, now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(trades) == 0 {
		w.scored[sym] = symScore{updatedAt: now}
		return
	}
	wins := 0
	pnl := 0.0
	rs := make([]float64, 0, len(trades))
	for _, t := range trades {
		pnl += t.PnL
		if t.PnL > 0 {
			wins++
		}
		rs = append(rs, t.PnL) // PnL acts as proxy R-unit when sizes are equal-risk
	}
	mean := 0.0
	for _, r := range rs {
		mean += r
	}
	mean /= float64(len(rs))
	v := 0.0
	for _, r := range rs {
		d := r - mean
		v += d * d
	}
	std := 0.0
	if len(rs) > 1 {
		std = sqrtSafe(v / float64(len(rs)-1))
	}
	sharpe := 0.0
	if std > 0 {
		// daily-cadence approx; multiply by sqrt(N) for an N-period scale
		sharpe = mean / std
	}
	w.scored[sym] = symScore{
		sharpe:    sharpe,
		winRate:   float64(wins) / float64(len(trades)),
		pnl:       pnl,
		nTrades:   len(trades),
		updatedAt: now,
	}
}

func sqrtSafe(x float64) float64 {
	if x <= 0 {
		return 0
	}
	// Newton-Raphson, 6 iters is plenty for our scale
	g := x / 2
	for i := 0; i < 8; i++ {
		g = 0.5 * (g + x/g)
	}
	return g
}

// Allowed reports whether the symbol currently passes the threshold.
// If no score exists for the symbol, returns false (be safe).
func (w *Whitelist) Allowed(sym string) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	s, ok := w.scored[sym]
	if !ok {
		return false
	}
	return s.nTrades >= w.minTrades && s.sharpe > w.sharpeMin
}

// AllowedOrFallback returns the symbols above threshold; if the result is
// empty it falls back to the top-K symbols by Sharpe (to avoid sitting idle).
func (w *Whitelist) AllowedOrFallback(syms []string, fallbackTopK int) []string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	type kv struct {
		s string
		v float64
	}
	all := make([]kv, 0, len(syms))
	approved := make([]string, 0, len(syms))
	for _, s := range syms {
		sc, ok := w.scored[s]
		if !ok {
			continue
		}
		all = append(all, kv{s, sc.sharpe})
		if sc.nTrades >= w.minTrades && sc.sharpe > w.sharpeMin {
			approved = append(approved, s)
		}
	}
	if len(approved) > 0 {
		return approved
	}
	sort.Slice(all, func(i, j int) bool { return all[i].v > all[j].v })
	out := make([]string, 0, fallbackTopK)
	for _, kv := range all {
		if kv.v <= 0 {
			break
		}
		out = append(out, kv.s)
		if len(out) >= fallbackTopK {
			break
		}
	}
	return out
}

// Snapshot returns a copy of the current scoreboard for monitoring.
func (w *Whitelist) Snapshot() map[string]struct {
	Sharpe  float64
	WinRate float64
	PnL     float64
	N       int
	Updated time.Time
} {
	w.mu.RLock()
	defer w.mu.RUnlock()
	out := make(map[string]struct {
		Sharpe  float64
		WinRate float64
		PnL     float64
		N       int
		Updated time.Time
	}, len(w.scored))
	for k, v := range w.scored {
		out[k] = struct {
			Sharpe  float64
			WinRate float64
			PnL     float64
			N       int
			Updated time.Time
		}{v.sharpe, v.winRate, v.pnl, v.nTrades, v.updatedAt}
	}
	return out
}
