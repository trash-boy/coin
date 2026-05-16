package backtest

import (
	"fmt"
	"math"
	"time"

	"coin/internal/strategy"
)

type Trade struct {
	EntryTime time.Time
	ExitTime  time.Time
	Side      strategy.Side
	Entry     float64
	Exit      float64
	Quantity  float64
	PnL       float64
	Fees      float64
	EntryFee  float64
	ExitFee   float64
	Reason    string
}

type Result struct {
	InitialEquity float64
	FinalEquity   float64
	ReturnPct     float64
	MaxDrawdown   float64
	FundingPaid   float64
	Trades        []Trade
	Segments      []Segment
	WinRate       float64
	ProfitFactor  float64
	LastSignal    strategy.Signal
}

type Segment struct {
	Name         string
	Trades       int
	PnL          float64
	WinRate      float64
	ProfitFactor float64
}

type position struct {
	side        strategy.Side
	entryTime   time.Time
	entryPrice  float64
	qty         float64
	entryFee    float64
	trail       float64
	riskPerUnit float64
	addCount    int
	lastAdd     float64
	lastAddBar  int
	leverage    float64
}

type pendingOrder struct {
	side        strategy.Side
	stop        float64
	atr         float64
	riskScale   float64
	fundingRate float64
	reason      string
	add         bool
	exit        bool
}

func Run(candles []strategy.Candle, initialEquity float64, cfg strategy.Config) (Result, error) {
	return RunWithFunding(candles, initialEquity, cfg, nil)
}

func RunWithFunding(candles []strategy.Candle, initialEquity float64, cfg strategy.Config, funding []strategy.FundingRate) (Result, error) {
	if err := cfg.Validate(); err != nil {
		return Result{}, err
	}
	if initialEquity <= 0 {
		return Result{}, fmt.Errorf("initial equity must be positive")
	}
	if len(candles) < strategy.WarmupBars(cfg)+2 {
		return Result{}, fmt.Errorf("need at least %d candles, got %d", strategy.WarmupBars(cfg)+2, len(candles))
	}

	ind := strategy.BuildIndicators(candles, cfg)
	warmup := strategy.WarmupBars(cfg)
	balance := initialEquity
	peakEquity := initialEquity
	maxDDPct := 0.0
	dayStartEquity := initialEquity
	currentDay := tradingDay(candles[warmup].OpenTime)
	cooldown := 0
	haltedToday := false
	haltedPermanent := false
	fundingIndex := 0
	latestFundingRate := 0.0
	fundingPaid := 0.0

	for fundingIndex < len(funding) && !funding[fundingIndex].Time.After(candles[warmup].CloseTime) {
		latestFundingRate = funding[fundingIndex].Rate
		fundingIndex++
	}

	pos := position{side: strategy.Flat}
	trades := make([]Trade, 0)
	var pending *pendingOrder

	for i := warmup; i < len(candles); i++ {
		c := candles[i]
		day := tradingDay(c.OpenTime)
		if day != currentDay {
			currentDay = day
			dayStartEquity = markEquity(balance, pos, c.Open)
			haltedToday = false
		}

		if pending != nil && !haltedToday && !haltedPermanent && cooldown == 0 {
			if pending.exit && pos.side != strategy.Flat {
				balance, trades = closePosition(balance, pos, c.Open, c.OpenTime, cfg, trades, "next-open exit: "+pending.reason)
				pos = position{side: strategy.Flat}
				cooldown = cfg.CooldownBars
			} else if pending.add && pos.side == pending.side {
				var added bool
				equity := markEquity(balance, pos, c.Open)
				balance, pos, added = executeAdd(balance, equity, pos, pending, c, i, cfg)
				if added {
					cooldown = 1
				}
			} else if !pending.add && pos.side == strategy.Flat {
				var opened bool
				equity := markEquity(balance, pos, c.Open)
				balance, pos, opened = executeEntry(balance, equity, pending, c, cfg)
				if !opened {
					pending = nil
				}
			}
		}
		pending = nil

		for fundingIndex < len(funding) && !funding[fundingIndex].Time.After(c.CloseTime) {
			latestFundingRate = funding[fundingIndex].Rate
			if pos.side != strategy.Flat {
				mark := funding[fundingIndex].MarkPrice
				if mark <= 0 {
					mark = c.Close
				}
				cost := fundingCost(pos, mark, latestFundingRate)
				balance -= cost
				fundingPaid += cost
			}
			fundingIndex++
		}

		if pos.side != strategy.Flat {
			var exited bool
			balance, trades, exited = maybeExitIntrabar(balance, pos, c, cfg, trades)
			if exited {
				pos = position{side: strategy.Flat}
				cooldown = cfg.CooldownBars
			}
		}

		if pos.side != strategy.Flat {
			pos = updateTrailingStop(pos, c.Close, ind.ATR[i], cfg)
		}

		equity := markEquity(balance, pos, c.Close)
		if equity > peakEquity {
			peakEquity = equity
		}
		drawdown := 1 - equity/peakEquity
		if drawdown > maxDDPct {
			maxDDPct = drawdown
		}
		dayLoss := 1 - equity/dayStartEquity

		if drawdown >= cfg.MaxDrawdown {
			if pos.side != strategy.Flat {
				balance, trades = closePosition(balance, pos, c.Close, c.CloseTime, cfg, trades, "max drawdown stop")
				pos = position{side: strategy.Flat}
			}
			haltedPermanent = true
		}
		if dayLoss >= cfg.DailyLossLimit {
			if pos.side != strategy.Flat {
				balance, trades = closePosition(balance, pos, c.Close, c.CloseTime, cfg, trades, "daily loss stop")
				pos = position{side: strategy.Flat}
			}
			haltedToday = true
		}
		if haltedPermanent {
			break
		}

		if cooldown > 0 {
			cooldown--
			continue
		}
		if haltedToday || i == len(candles)-1 {
			continue
		}

		riskScale := drawdownScale(drawdown, cfg) * kellyScale(trades, cfg)
		side, reason := strategy.EntrySide(candles, ind, i, cfg, latestFundingRate)
		if pos.side != strategy.Flat {
			if side != strategy.Flat && side != pos.side {
				pending = &pendingOrder{exit: true, side: pos.side, reason: reason}
				continue
			}
			if side == pos.side && canPyramid(pos, c.Close, ind.ATR[i], i, cfg) {
				stop, ok, _ := strategy.StructureStop(candles, ind, i, side, cfg)
				if ok {
					pending = &pendingOrder{side: side, stop: stop, atr: ind.ATR[i], riskScale: riskScale * cfg.PyramidRiskScale, fundingRate: latestFundingRate, reason: reason, add: true}
				}
			}
			continue
		}
		if side == strategy.Flat {
			continue
		}
		stop, ok, _ := strategy.StructureStop(candles, ind, i, side, cfg)
		if !ok {
			continue
		}
		pending = &pendingOrder{side: side, stop: stop, atr: ind.ATR[i], riskScale: riskScale, fundingRate: latestFundingRate, reason: reason}
	}

	last := candles[len(candles)-1]
	if pos.side != strategy.Flat {
		balance, trades = closePosition(balance, pos, last.Close, last.CloseTime, cfg, trades, "end of backtest")
	}

	lastSignal, _ := strategy.LatestSignal(candles, strategy.Context{Equity: balance, FundingRate: latestFundingRate}, cfg)
	result := Result{
		InitialEquity: initialEquity,
		FinalEquity:   balance,
		ReturnPct:     (balance/initialEquity - 1) * 100,
		MaxDrawdown:   maxDDPct * 100,
		FundingPaid:   fundingPaid,
		Trades:        trades,
		Segments:      segmentStats(trades),
		LastSignal:    lastSignal,
	}
	result.WinRate, result.ProfitFactor = tradeStats(trades)
	return result, nil
}

func executeEntry(balance, equity float64, pending *pendingOrder, c strategy.Candle, cfg strategy.Config) (float64, position, bool) {
	if entryViolatesStop(c.Open, pending.side, pending.stop) {
		return balance, position{side: strategy.Flat}, false
	}
	execCandle := c
	execCandle.Close = c.Open
	execCandle.CloseTime = c.OpenTime
	sig := strategy.BuildEntrySignal(execCandle, pending.side, equity, pending.atr, pending.stop, pending.riskScale, pending.fundingRate, cfg)
	if sig.Action != strategy.ActionEnter || sig.Quantity <= 0 || sig.Notional <= 0 {
		return balance, position{side: strategy.Flat}, false
	}
	pos := openPosition(sig, cfg)
	balance -= pos.entryFee
	return balance, pos, true
}

func executeAdd(balance, equity float64, pos position, pending *pendingOrder, c strategy.Candle, barIndex int, cfg strategy.Config) (float64, position, bool) {
	if entryViolatesStop(c.Open, pending.side, pending.stop) {
		return balance, pos, false
	}
	stop := pending.stop
	if pending.side == strategy.Long && stop < pos.trail {
		stop = pos.trail
	}
	if pending.side == strategy.Short && stop > pos.trail {
		stop = pos.trail
	}
	execCandle := c
	execCandle.Close = c.Open
	execCandle.CloseTime = c.OpenTime
	sig := strategy.BuildEntrySignal(execCandle, pending.side, equity, pending.atr, stop, pending.riskScale, pending.fundingRate, cfg)
	if sig.Action != strategy.ActionEnter || sig.Quantity <= 0 || sig.Notional <= 0 {
		return balance, pos, false
	}
	currentNotional := pos.qty * c.Open
	maxNotional := equity * strategy.EffectiveLeverage(c.Open, pending.atr, cfg) * cfg.MaxMarginUse
	remaining := maxNotional - currentNotional
	if remaining <= 0 {
		return balance, pos, false
	}
	if sig.Notional > remaining {
		sig.Notional = remaining
		sig.Quantity = remaining / c.Open
	}

	addPrice := applyEntrySlippage(sig.Price, pending.side, cfg)
	addFee := addPrice * sig.Quantity * cfg.FeeRate
	newQty := pos.qty + sig.Quantity
	pos.entryPrice = (pos.entryPrice*pos.qty + addPrice*sig.Quantity) / newQty
	pos.qty = newQty
	pos.entryFee += addFee
	pos.addCount++
	pos.lastAdd = addPrice
	pos.lastAddBar = barIndex
	if pending.side == strategy.Long && stop > pos.trail {
		pos.trail = stop
	}
	if pending.side == strategy.Short && stop < pos.trail {
		pos.trail = stop
	}
	pos.riskPerUnit = math.Max(math.Abs(pos.entryPrice-pos.trail), pending.atr*cfg.MinStopATR)
	pos.leverage = sig.Leverage
	balance -= addFee
	return balance, pos, true
}

func openPosition(sig strategy.Signal, cfg strategy.Config) position {
	entryPrice := applyEntrySlippage(sig.Price, sig.Side, cfg)
	entryFee := entryPrice * sig.Quantity * cfg.FeeRate
	risk := math.Abs(entryPrice - sig.StopLoss)
	return position{
		side:        sig.Side,
		entryTime:   sig.Time,
		entryPrice:  entryPrice,
		qty:         sig.Quantity,
		entryFee:    entryFee,
		trail:       sig.StopLoss,
		riskPerUnit: risk,
		lastAdd:     entryPrice,
		leverage:    sig.Leverage,
	}
}

func maybeExitIntrabar(balance float64, pos position, c strategy.Candle, cfg strategy.Config, trades []Trade) (float64, []Trade, bool) {
	if pos.side == strategy.Long {
		if c.Open <= pos.trail {
			b, ts := closePosition(balance, pos, c.Open, c.OpenTime, cfg, trades, "gap stop/trailing stop")
			return b, ts, true
		}
		if c.Low <= pos.trail {
			b, ts := closePosition(balance, pos, pos.trail, c.CloseTime, cfg, trades, "stop/trailing stop")
			return b, ts, true
		}
	}
	if pos.side == strategy.Short {
		if c.Open >= pos.trail {
			b, ts := closePosition(balance, pos, c.Open, c.OpenTime, cfg, trades, "gap stop/trailing stop")
			return b, ts, true
		}
		if c.High >= pos.trail {
			b, ts := closePosition(balance, pos, pos.trail, c.CloseTime, cfg, trades, "stop/trailing stop")
			return b, ts, true
		}
	}
	return balance, trades, false
}

func updateTrailingStop(pos position, closePrice, atr float64, cfg strategy.Config) position {
	if pos.riskPerUnit <= 0 || atr <= 0 {
		return pos
	}
	if pos.side == strategy.Long {
		rMultiple := (closePrice - pos.entryPrice) / pos.riskPerUnit
		if rMultiple >= cfg.BreakEvenR && pos.entryPrice > pos.trail {
			pos.trail = pos.entryPrice
		}
		if rMultiple >= cfg.TrailActivationR {
			next := closePrice - atr*cfg.TrailATR
			if next > pos.trail {
				pos.trail = next
			}
		}
	}
	if pos.side == strategy.Short {
		rMultiple := (pos.entryPrice - closePrice) / pos.riskPerUnit
		if rMultiple >= cfg.BreakEvenR && pos.entryPrice < pos.trail {
			pos.trail = pos.entryPrice
		}
		if rMultiple >= cfg.TrailActivationR {
			next := closePrice + atr*cfg.TrailATR
			if next < pos.trail {
				pos.trail = next
			}
		}
	}
	return pos
}

func canPyramid(pos position, closePrice, atr float64, barIndex int, cfg strategy.Config) bool {
	if pos.addCount >= cfg.MaxAdds || atr <= 0 {
		return false
	}
	if pos.addCount > 0 && barIndex-pos.lastAddBar < cfg.MinAddSpacingBars {
		return false
	}
	if pos.side == strategy.Long {
		return closePrice-pos.lastAdd >= cfg.PyramidATR*atr
	}
	if pos.side == strategy.Short {
		return pos.lastAdd-closePrice >= cfg.PyramidATR*atr
	}
	return false
}

func closePosition(balance float64, pos position, price float64, t time.Time, cfg strategy.Config, trades []Trade, reason string) (float64, []Trade) {
	exitPrice := applyExitSlippage(price, pos.side, cfg)
	var gross float64
	if pos.side == strategy.Long {
		gross = (exitPrice - pos.entryPrice) * pos.qty
	} else {
		gross = (pos.entryPrice - exitPrice) * pos.qty
	}
	exitFee := exitPrice * pos.qty * cfg.FeeRate
	net := gross - pos.entryFee - exitFee
	balance += gross - exitFee
	trades = append(trades, Trade{
		EntryTime: pos.entryTime,
		ExitTime:  t,
		Side:      pos.side,
		Entry:     pos.entryPrice,
		Exit:      exitPrice,
		Quantity:  pos.qty,
		PnL:       net,
		Fees:      pos.entryFee + exitFee,
		EntryFee:  pos.entryFee,
		ExitFee:   exitFee,
		Reason:    reason,
	})
	return balance, trades
}

func markEquity(balance float64, pos position, price float64) float64 {
	if pos.side == strategy.Flat {
		return balance
	}
	if pos.side == strategy.Long {
		return balance + (price-pos.entryPrice)*pos.qty
	}
	return balance + (pos.entryPrice-price)*pos.qty
}

func fundingCost(pos position, price, rate float64) float64 {
	notional := pos.qty * price
	if pos.side == strategy.Long {
		return notional * rate
	}
	if pos.side == strategy.Short {
		return -notional * rate
	}
	return 0
}

func drawdownScale(drawdown float64, cfg strategy.Config) float64 {
	if drawdown >= cfg.DDReduce2 {
		return cfg.DDScale2
	}
	if drawdown >= cfg.DDReduce1 {
		return cfg.DDScale1
	}
	return 1
}

func kellyScale(trades []Trade, cfg strategy.Config) float64 {
	if len(trades) < cfg.KellyLookback || cfg.KellyFraction == 0 {
		return 1
	}
	start := len(trades) - cfg.KellyLookback
	if start < 0 {
		start = 0
	}
	wins := 0
	winTotal := 0.0
	lossTotal := 0.0
	losses := 0
	for _, tr := range trades[start:] {
		if tr.PnL > 0 {
			wins++
			winTotal += tr.PnL
		} else if tr.PnL < 0 {
			losses++
			lossTotal += -tr.PnL
		}
	}
	if wins == 0 {
		return cfg.MinKellyScale
	}
	if losses == 0 || lossTotal == 0 {
		return cfg.MaxKellyScale
	}
	p := float64(wins) / float64(wins+losses)
	b := (winTotal / float64(wins)) / (lossTotal / float64(losses))
	kelly := p - (1-p)/b
	if kelly <= 0 {
		return cfg.MinKellyScale
	}
	scale := kelly * cfg.KellyFraction
	if scale < cfg.MinKellyScale {
		return cfg.MinKellyScale
	}
	if scale > cfg.MaxKellyScale {
		return cfg.MaxKellyScale
	}
	return scale
}

func entryViolatesStop(price float64, side strategy.Side, stop float64) bool {
	return (side == strategy.Long && price <= stop) || (side == strategy.Short && price >= stop)
}

func tradingDay(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

func applyEntrySlippage(price float64, side strategy.Side, cfg strategy.Config) float64 {
	slip := cfg.SlippageBps / 10000
	if side == strategy.Long {
		return price * (1 + slip)
	}
	return price * (1 - slip)
}

func applyExitSlippage(price float64, side strategy.Side, cfg strategy.Config) float64 {
	slip := cfg.SlippageBps / 10000
	if side == strategy.Long {
		return price * (1 - slip)
	}
	return price * (1 + slip)
}

func tradeStats(trades []Trade) (float64, float64) {
	if len(trades) == 0 {
		return 0, 0
	}
	wins := 0
	grossWin := 0.0
	grossLoss := 0.0
	for _, tr := range trades {
		if tr.PnL > 0 {
			wins++
			grossWin += tr.PnL
		} else {
			grossLoss += -tr.PnL
		}
	}
	pf := 0.0
	if grossLoss > 0 {
		pf = grossWin / grossLoss
	}
	return float64(wins) / float64(len(trades)) * 100, pf
}

func segmentStats(trades []Trade) []Segment {
	type bucket struct {
		trades []Trade
		pnl    float64
	}
	buckets := make(map[string]*bucket)
	order := make([]string, 0)
	for _, tr := range trades {
		key := tr.ExitTime.UTC().Format("2006")
		if _, ok := buckets[key]; !ok {
			buckets[key] = &bucket{}
			order = append(order, key)
		}
		buckets[key].trades = append(buckets[key].trades, tr)
		buckets[key].pnl += tr.PnL
	}
	out := make([]Segment, 0, len(order))
	for _, key := range order {
		winRate, profitFactor := tradeStats(buckets[key].trades)
		out = append(out, Segment{
			Name:         key,
			Trades:       len(buckets[key].trades),
			PnL:          buckets[key].pnl,
			WinRate:      winRate,
			ProfitFactor: profitFactor,
		})
	}
	return out
}
