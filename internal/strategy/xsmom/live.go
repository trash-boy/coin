package xsmom

import (
	"fmt"
	"sort"
	"time"

	"coin/internal/strategy"
)

// Target represents one position the strategy wants to hold after the
// next rebalance. Used by the live runner to compute order deltas.
type Target struct {
	Symbol     string
	Side       strategy.Side
	WeightFrac float64 // fraction of equity (positive)
	Notional   float64 // WeightFrac * equity
}

// SignalSet is the full output of one rebalance decision.
type SignalSet struct {
	GeneratedAt    time.Time     // == latest day in the panel
	RebalanceDay   time.Time
	UniverseSize   int
	Targets        []Target
	Reason         string
	Skipped        bool   // true when universe too small or signal disabled
	SkipReason     string
	LongCandidates []RankedReturn
	ShortCandidates []RankedReturn
}

// LatestSignal computes the target portfolio from the most recent day
// in the panel. Caller is responsible for deciding whether *today* is
// actually a rebalance day (typical pattern: anchor rebalance to a
// fixed weekday and call this on that weekday).
func LatestSignal(p *Panel, cfg Config, equity float64) (SignalSet, error) {
	if err := p.EnsureCompatible(); err != nil {
		return SignalSet{}, err
	}
	if err := cfg.Validate(); err != nil {
		return SignalSet{}, err
	}
	if equity <= 0 {
		return SignalSet{}, fmt.Errorf("xsmom: equity must be positive")
	}
	i := len(p.Days) - 1
	out := SignalSet{
		GeneratedAt:  p.Days[i],
		RebalanceDay: p.Days[i],
	}
	elig := p.EligibleAt(i, cfg)
	out.UniverseSize = len(elig)

	longs, shorts := p.SelectLongShort(i, cfg)
	out.LongCandidates = longs
	out.ShortCandidates = shorts
	if longs == nil && shorts == nil {
		out.Skipped = true
		out.SkipReason = fmt.Sprintf("universe size %d < required %d (top+bot+5)",
			len(elig), cfg.TopN+cfg.BotN+5)
		return out, nil
	}

	wLong, wShort := cfg.LegWeights()
	for _, r := range longs {
		out.Targets = append(out.Targets, Target{
			Symbol:     r.Symbol,
			Side:       strategy.Long,
			WeightFrac: wLong,
			Notional:   wLong * equity,
		})
	}
	for _, r := range shorts {
		out.Targets = append(out.Targets, Target{
			Symbol:     r.Symbol,
			Side:       strategy.Short,
			WeightFrac: wShort,
			Notional:   wShort * equity,
		})
	}
	sort.Slice(out.Targets, func(a, b int) bool {
		if out.Targets[a].Side != out.Targets[b].Side {
			return out.Targets[a].Side == strategy.Long
		}
		return out.Targets[a].Symbol < out.Targets[b].Symbol
	})
	out.Reason = fmt.Sprintf("xsmom L%d/S%d lookback=%dd hold=%dd lev=%.2fx universe=%d",
		cfg.TopN, cfg.BotN, cfg.LookbackDays, cfg.HoldDays, cfg.Leverage, len(elig))
	return out, nil
}

// OrderDelta describes the difference between the current open
// positions and the target portfolio. Use this to drive order routing.
type OrderDelta struct {
	Symbol      string
	Side        strategy.Side // target side; FLAT means close
	DeltaQty    float64       // positive: buy/long-add or short-cover; negative: sell/long-trim or short-add
	TargetNotional float64
	Reason      string
}

// CurrentPosition is the externally-known open position used as
// reference when computing OrderDelta.
type CurrentPosition struct {
	Symbol   string
	Side     strategy.Side // FLAT, LONG or SHORT
	Notional float64       // current absolute notional in USDT
}

// ComputeOrderDeltas produces the minimum set of orders required to
// move from `current` positions to `signal.Targets`. Symbols not in
// targets but currently held are flattened.
//
// Notes:
//   - Quantity is left at 0; caller must convert TargetNotional to
//     base-asset quantity using the latest mark/last price and the
//     exchange step size (binance.SymbolRules.RoundQuantity).
//   - Existing positions whose side disagrees with the target trigger
//     a single cross-side delta with TargetNotional = sum of close +
//     open. The router is expected to send two orders (close, then
//     open) — see live runner for the typical implementation.
func ComputeOrderDeltas(signal SignalSet, current []CurrentPosition) []OrderDelta {
	if signal.Skipped {
		return nil
	}
	tgtBy := map[string]Target{}
	for _, t := range signal.Targets {
		tgtBy[t.Symbol] = t
	}
	curBy := map[string]CurrentPosition{}
	for _, c := range current {
		curBy[c.Symbol] = c
	}
	out := make([]OrderDelta, 0)
	// Flatten symbols no longer in target.
	for sym, c := range curBy {
		if _, in := tgtBy[sym]; in {
			continue
		}
		if c.Side == strategy.Flat || c.Notional == 0 {
			continue
		}
		out = append(out, OrderDelta{
			Symbol:         sym,
			Side:           strategy.Flat,
			DeltaQty:       0,
			TargetNotional: 0,
			Reason:         "exit: dropped from xsmom basket",
		})
	}
	for sym, t := range tgtBy {
		c, has := curBy[sym]
		if !has || c.Side == strategy.Flat || c.Notional == 0 {
			out = append(out, OrderDelta{
				Symbol:         sym,
				Side:           t.Side,
				DeltaQty:       0,
				TargetNotional: t.Notional,
				Reason:         "open: new entry to xsmom basket",
			})
			continue
		}
		if c.Side != t.Side {
			out = append(out, OrderDelta{
				Symbol:         sym,
				Side:           t.Side,
				DeltaQty:       0,
				TargetNotional: c.Notional + t.Notional,
				Reason:         "flip: side mismatch (close+open)",
			})
			continue
		}
		// Same side: only resize when |delta| > 5% of target notional.
		diff := t.Notional - c.Notional
		threshold := 0.05 * t.Notional
		if absF(diff) <= threshold {
			continue
		}
		out = append(out, OrderDelta{
			Symbol:         sym,
			Side:           t.Side,
			DeltaQty:       0,
			TargetNotional: t.Notional,
			Reason:         fmt.Sprintf("resize: notional %.2f -> %.2f", c.Notional, t.Notional),
		})
	}
	sort.Slice(out, func(a, b int) bool {
		// Close first, then opens.
		ai := orderRank(out[a])
		bi := orderRank(out[b])
		if ai != bi {
			return ai < bi
		}
		return out[a].Symbol < out[b].Symbol
	})
	return out
}

func orderRank(o OrderDelta) int {
	switch o.Side {
	case strategy.Flat:
		return 0
	case strategy.Long:
		return 2
	case strategy.Short:
		return 3
	default:
		return 1
	}
}

func absF(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
