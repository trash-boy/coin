package m15v2

import (
	"math"
	"time"
)

// Strategy is the stateful 15m engine.
//
// Lifecycle (per 15m closed bar):
//   1) call OnBar(bar, htf, equity, now, events)
//   2) consume the returned []Signal, route to executor
//   3) on fills/closes, call OnFill / OnClose to keep state aligned
type Strategy struct {
	P    Params
	Ind  *Indicators
	F    *Filter
	R    *Risk

	Pos     *Position     // nil if flat
	Pending *PendingEntry // nil if no live limit entry

	HTF    HTFState
	Events []EventWindow
}

// New builds a strategy with default-or-supplied params.
func New(p Params) *Strategy {
	return &Strategy{
		P:   p,
		Ind: NewIndicators(p),
		F:   NewFilter(p),
		R:   NewRisk(p),
	}
}

// SetHTF updates the cached 1h regime view.
func (s *Strategy) SetHTF(h HTFState) { s.HTF = h }

// SetEvents replaces the event blackout list.
func (s *Strategy) SetEvents(e []EventWindow) { s.Events = e }

// OnBar drives the strategy through one freshly-closed 15m bar and returns
// any signals the executor must act on.
func (s *Strategy) OnBar(bar Bar, equity float64) []Signal {
	out := make([]Signal, 0, 4)

	// 1) update indicators
	s.Ind.Update(bar)
	s.R.OnNewBar(bar.Time, equity)

	// 2) decay pending limit entry TTL; cancel if expired or trend changed
	if s.Pending != nil {
		s.Pending.BarsLeft--
		side := s.Pending.Side
		stillAligned := (side == SideLong && s.F.TrendOK(s.Ind) > 0) ||
			(side == SideShort && s.F.TrendOK(s.Ind) < 0)
		if s.Pending.BarsLeft <= 0 || !stillAligned {
			out = append(out, Signal{
				Time:   bar.Time,
				Action: ActCancelEntry,
				Side:   side,
				Reason: "limit-expired-or-trend-flip",
			})
			s.Pending = nil
		}
	}

	// 3) if in a position, age it and run exit ladder
	if s.Pos != nil {
		s.Pos.BarsHeld++
		if bar.High > s.Pos.HighWater {
			s.Pos.HighWater = bar.High
		}
		if s.Pos.LowWater == 0 || bar.Low < s.Pos.LowWater {
			s.Pos.LowWater = bar.Low
		}

		// loop: an exit may unlock another (e.g., TP1 -> trail update)
		for i := 0; i < 4; i++ {
			d := EvaluateExits(s.P, s.Ind, s.Pos, bar)
			if d.Action == ActNone {
				break
			}
			out = append(out, signalFromExit(bar.Time, s.Pos, d))
			applyExitToState(s.Pos, d)
			if s.Pos.Qty <= 0 {
				// fully closed; recording happens via OnClose
				break
			}
		}
	}

	// 4) if flat & no pending, try to open a new limit entry
	if s.Pos == nil && s.Pending == nil {
		if sig, ok := s.tryEntry(bar, equity); ok {
			out = append(out, sig)
		}
	}

	return out
}

// tryEntry evaluates the entry filters and, if all pass, returns an
// ActOpenLimit signal (and records the pending entry locally).
func (s *Strategy) tryEntry(bar Bar, equity float64) (Signal, bool) {
	if !s.Ind.Ready() {
		return Signal{}, false
	}
	if ok, _ := s.R.CanOpen(bar.Time); !ok {
		return Signal{}, false
	}
	if blocked, _ := EventBlocked(bar.Time, s.Events); blocked {
		return Signal{}, false
	}

	regime := s.F.TrendOK(s.Ind)
	if regime == 0 {
		return Signal{}, false
	}
	if !s.F.MomentumOK(s.Ind) {
		return Signal{}, false
	}
	if !s.F.SqueezeReady(s.Ind) {
		return Signal{}, false
	}

	var side Side
	if regime > 0 {
		side = SideLong
	} else {
		side = SideShort
	}
	if !s.F.HTFAligned(s.HTF, side) {
		return Signal{}, false
	}

	// Donchian breakout trigger on the just-closed bar
	switch side {
	case SideLong:
		if bar.Close < s.Ind.DonHi()*0.999 { // not breaking out
			return Signal{}, false
		}
	case SideShort:
		if bar.Close > s.Ind.DonLo()*1.001 {
			return Signal{}, false
		}
	}

	// Pullback limit price = EMA21 ± offset*ATR
	atr := s.Ind.ATR()
	if atr <= 0 {
		return Signal{}, false
	}
	var limitPx, stop float64
	switch side {
	case SideLong:
		limitPx = s.Ind.EMAPullback() + s.P.PullbackOff*atr
		// don't chase: limit must be <= bar.Close (we want a pullback)
		if limitPx >= bar.Close {
			limitPx = bar.Close - 0.05*atr
		}
		stop = limitPx - s.P.ATRStopK*atr
	case SideShort:
		limitPx = s.Ind.EMAPullback() - s.P.PullbackOff*atr
		if limitPx <= bar.Close {
			limitPx = bar.Close + 0.05*atr
		}
		stop = limitPx + s.P.ATRStopK*atr
	}

	qty := s.R.SizeQty(equity, limitPx, stop)
	if qty <= 0 {
		return Signal{}, false
	}

	// record pending locally
	s.Pending = &PendingEntry{
		Side:       side,
		LimitPrice: limitPx,
		Stop:       stop,
		SizeQty:    qty,
		BarsLeft:   s.P.LimitTTL,
		PlacedAt:   bar.Time,
	}

	return Signal{
		Time:     bar.Time,
		Action:   ActOpenLimit,
		Side:     side,
		Price:    limitPx,
		Stop:     stop,
		SizeQty:  qty,
		LimitTTL: s.P.LimitTTL,
		Reason:   "donchian-breakout+pullback",
	}, true
}

// OnFill should be invoked by the executor when the pending limit fills.
// fillPx may differ from LimitPrice due to slippage.
func (s *Strategy) OnFill(t time.Time, fillPx float64) {
	if s.Pending == nil {
		return
	}
	pe := s.Pending
	s.Pending = nil
	s.Pos = &Position{
		Side:     pe.Side,
		Entry:    fillPx,
		Stop:     pe.Stop,
		InitStop: pe.Stop,
		Qty:      pe.SizeQty,
		OpenTime: t,
		HighWater: fillPx,
		LowWater:  fillPx,
	}
}

// OnClose should be called when the position has been fully exited (Qty=0
// after partial-close cycles). The executor passes realized PnL in USDT.
func (s *Strategy) OnClose(t time.Time, realizedPnL, equity float64) {
	if s.Pos == nil {
		return
	}
	stat := TradeStat{
		Win:      realizedPnL > 0,
		PnL:      realizedPnL,
		ClosedAt: t,
	}
	s.R.RecordTrade(stat, equity)
	s.Pos = nil
}

// --- helpers ---

func signalFromExit(t time.Time, pos *Position, d ExitDecision) Signal {
	sig := Signal{
		Time:   t,
		Action: d.Action,
		Side:   pos.Side,
		Reason: d.Reason,
	}
	switch d.Action {
	case ActClosePartial:
		sig.Price = d.Price
		sig.Portion = d.Fraction
		sig.SizeQty = pos.Qty * d.Fraction
	case ActCloseAll:
		sig.Price = d.Price
		sig.SizeQty = pos.Qty
	case ActAdjustStop:
		sig.Stop = d.NewStop
	}
	return sig
}

func applyExitToState(pos *Position, d ExitDecision) {
	switch d.Action {
	case ActClosePartial:
		closed := pos.Qty * d.Fraction
		pos.Qty = math.Max(0, pos.Qty-closed)
		if d.Reason == "tp1" {
			pos.TP1Done = true
			// move stop to break-even at TP1
			pos.Stop = pos.Entry
		} else if d.Reason == "tp2" {
			pos.TP2Done = true
		}
	case ActCloseAll:
		pos.Qty = 0
	case ActAdjustStop:
		pos.Stop = d.NewStop
	}
}
