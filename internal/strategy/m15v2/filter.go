package m15v2

import "time"

// HTFState holds the pre-computed higher-timeframe (1h) bias.
// The host system is responsible for keeping this fresh; the strategy only reads it.
type HTFState struct {
	EMAFast float64
	EMASlow float64
	Close   float64
	Updated time.Time
}

// Bullish reports whether the 1h regime favors longs.
func (h HTFState) Bullish() bool {
	return h.Close > h.EMAFast && h.EMAFast > h.EMASlow
}

// Bearish reports whether the 1h regime favors shorts.
func (h HTFState) Bearish() bool {
	return h.Close < h.EMAFast && h.EMAFast < h.EMASlow
}

// Filter aggregates entry-side filters for the m15v2 strategy.
type Filter struct {
	p Params
}

func NewFilter(p Params) *Filter { return &Filter{p: p} }

// TrendOK returns the EMA alignment regime based on 15m EMAs.
//   +1 = bullish stack, -1 = bearish stack, 0 = mixed.
func (f *Filter) TrendOK(ind *Indicators) int {
	if !ind.Ready() {
		return 0
	}
	if ind.EMAFast() > ind.EMAMid() && ind.EMAMid() > ind.EMASlow() {
		return +1
	}
	if ind.EMAFast() < ind.EMAMid() && ind.EMAMid() < ind.EMASlow() {
		return -1
	}
	return 0
}

// MomentumOK requires ADX above the configured threshold.
func (f *Filter) MomentumOK(ind *Indicators) bool {
	return ind.ADX() >= f.p.ADXMin
}

// SqueezeReady reports whether recent volatility is compressed enough that a
// breakout is statistically more likely to follow-through.
func (f *Filter) SqueezeReady(ind *Indicators) bool {
	return ind.SqueezePercentile() <= f.p.SqueezePct
}

// HTFAligned ensures the 1h bias matches the proposed direction.
func (f *Filter) HTFAligned(htf HTFState, side Side) bool {
	if !f.p.UseHTF {
		return true
	}
	switch side {
	case SideLong:
		return htf.Bullish()
	case SideShort:
		return htf.Bearish()
	}
	return false
}

// EventBlocked allows the host to inject blackout windows (FOMC, CPI, listing
// events, exchange maintenance). Empty list = no block.
type EventWindow struct {
	Start, End time.Time
	Reason     string
}

func EventBlocked(now time.Time, windows []EventWindow) (bool, string) {
	for _, w := range windows {
		if !now.Before(w.Start) && now.Before(w.End) {
			return true, w.Reason
		}
	}
	return false, ""
}
