package m15v2

import "math"

// ExitDecision is the result of evaluating exits on a bar.
type ExitDecision struct {
	Action    Action
	Price     float64 // intended price
	NewStop   float64 // for ActAdjustStop
	Fraction  float64 // for ActClosePartial (0..1 of remaining qty)
	Reason    string
}

// EvaluateExits checks TP1, TP2, time exit, donchian-trail update and reverse
// stop-out for the given bar. The strategy code drives ordering: this function
// returns at most ONE decision per call, and the caller may invoke it multiple
// times against the same bar (e.g., TP1 -> trail update) until it returns
// ActNone.
func EvaluateExits(p Params, ind *Indicators, pos *Position, bar Bar) ExitDecision {
	if pos == nil || pos.Side == SideNone || pos.Qty <= 0 {
		return ExitDecision{Action: ActNone}
	}

	// 1) hard stop hit during this bar
	if pos.Side == SideLong && bar.Low <= pos.Stop {
		return ExitDecision{Action: ActCloseAll, Price: pos.Stop, Reason: "stop"}
	}
	if pos.Side == SideShort && bar.High >= pos.Stop {
		return ExitDecision{Action: ActCloseAll, Price: pos.Stop, Reason: "stop"}
	}

	// 2) TP1
	if !pos.TP1Done {
		tp1 := tpPrice(pos, p.TP1R)
		if (pos.Side == SideLong && bar.High >= tp1) ||
			(pos.Side == SideShort && bar.Low <= tp1) {
			return ExitDecision{
				Action:   ActClosePartial,
				Price:    tp1,
				Fraction: p.TP1PartFrac,
				Reason:   "tp1",
			}
		}
	}

	// 3) TP2
	if pos.TP1Done && !pos.TP2Done && p.TP2PartFrac > 0 {
		tp2 := tpPrice(pos, p.TP2R)
		if (pos.Side == SideLong && bar.High >= tp2) ||
			(pos.Side == SideShort && bar.Low <= tp2) {
			// fraction of REMAINING qty
			rem := pos.Qty / math.Max(1e-12, 1-p.TP1PartFrac)
			_ = rem
			frac := p.TP2PartFrac / math.Max(1e-12, 1-p.TP1PartFrac)
			if frac > 1 {
				frac = 1
			}
			return ExitDecision{
				Action:   ActClosePartial,
				Price:    tp2,
				Fraction: frac,
				Reason:   "tp2",
			}
		}
	}

	// 4) Donchian-trail update once TP1 is done
	if pos.TP1Done && ind.Ready() {
		var newStop float64
		switch pos.Side {
		case SideLong:
			newStop = ind.DonTrailLo()
			if newStop > pos.Stop {
				return ExitDecision{Action: ActAdjustStop, NewStop: newStop, Reason: "trail"}
			}
		case SideShort:
			newStop = ind.DonTrailHi()
			if newStop < pos.Stop && newStop > 0 {
				return ExitDecision{Action: ActAdjustStop, NewStop: newStop, Reason: "trail"}
			}
		}
	}

	// 5) Reverse EMA cross — defensive exit if trend flips against us
	switch pos.Side {
	case SideLong:
		if ind.EMAFast() < ind.EMAMid() {
			return ExitDecision{Action: ActCloseAll, Price: bar.Close, Reason: "trend-flip"}
		}
	case SideShort:
		if ind.EMAFast() > ind.EMAMid() {
			return ExitDecision{Action: ActCloseAll, Price: bar.Close, Reason: "trend-flip"}
		}
	}

	// 6) Time exit
	if p.UseTimeExit && pos.BarsHeld >= p.MaxBarsHold {
		return ExitDecision{Action: ActCloseAll, Price: bar.Close, Reason: "time"}
	}

	return ExitDecision{Action: ActNone}
}

func tpPrice(pos *Position, rMult float64) float64 {
	r := math.Abs(pos.Entry - pos.InitStop)
	if pos.Side == SideLong {
		return pos.Entry + rMult*r
	}
	return pos.Entry - rMult*r
}
