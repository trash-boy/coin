package xsmom

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Result aggregates the backtest output.
type Result struct {
	Days            int
	StartDate       time.Time
	EndDate         time.Time
	StartIdx        int
	EndIdx          int
	InitialEquity   float64
	FinalEquity     float64
	TotalReturn     float64 // FinalEquity/InitialEquity - 1
	AnnReturn       float64 // (FinalEquity/InitialEquity)^(365/Days) - 1
	Sharpe          float64 // mean(daily_ret)/std(daily_ret) * sqrt(365)
	MaxDrawdown     float64 // negative number, e.g. -0.28
	NumRebalances   int
	AvgUniverse     float64
	FundingPaidPct  float64 // total funding paid in equity %, longs subtract
	DailyReturns    []float64
	EquityCurve     []float64 // length = Days+1, equity[0] = InitialEquity
	RebalanceDates  []time.Time
	SymbolTurnover  float64 // mean per-leg fraction of basket replaced per rebalance
	Trades          []Trade // long entries / exits and short entries / exits
	WindowEquity    [][]WindowSummary
}

// WindowSummary is reserved for future per-window analytics.
type WindowSummary struct {
	Start time.Time
	End   time.Time
	Eq    float64
}

// Trade records one rebalance event.
type Trade struct {
	Time          time.Time
	NewLongs      []string
	NewShorts     []string
	DroppedLongs  []string
	DroppedShorts []string
	CostFraction  float64 // turnover * (fee+slip) * 2 * leverage
}

// Run executes the cross-sectional momentum backtest over the panel
// from startIdx (inclusive) to endIdx (exclusive). Use startIdx <=0 to
// auto-pick the smallest valid index.
func Run(p *Panel, cfg Config, startIdx, endIdx int, initialEquity float64) (Result, error) {
	if err := p.EnsureCompatible(); err != nil {
		return Result{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Result{}, err
	}
	nDays := len(p.Days)
	minStart := cfg.MinHistDays
	if cfg.LookbackDays > minStart {
		minStart = cfg.LookbackDays
	}
	if startIdx <= 0 || startIdx < minStart {
		startIdx = minStart
	}
	if endIdx <= 0 || endIdx > nDays {
		endIdx = nDays
	}
	if startIdx >= endIdx {
		return Result{}, fmt.Errorf("xsmom: startIdx %d >= endIdx %d", startIdx, endIdx)
	}

	wLong, wShort := cfg.LegWeights()
	feeSlip := (cfg.FeeBps + cfg.SlipBps) / 1e4

	eq := initialEquity
	peak := eq
	maxDD := 0.0
	dailyRet := make([]float64, 0, endIdx-startIdx)
	equityCurve := make([]float64, 0, endIdx-startIdx+1)
	equityCurve = append(equityCurve, eq)
	fundingTotal := 0.0
	universes := make([]int, 0)
	rebalances := make([]time.Time, 0)
	trades := make([]Trade, 0)
	turnoverSum := 0.0
	turnoverN := 0

	curLongs := map[string]float64{}
	curShorts := map[string]float64{}

	for i := startIdx; i < endIdx; i++ {
		// 1) mark-to-market against previous close (skip first iter)
		if i > startIdx {
			prev := i - 1
			dr := 0.0
			for sym, w := range curLongs {
				j := indexOf(p.Symbols, sym)
				if j < 0 {
					continue
				}
				pn := p.close[i][j]
				pp := p.close[prev][j]
				if math.IsNaN(pn) || math.IsNaN(pp) || pp <= 0 {
					continue
				}
				dr += w * (pn/pp - 1)
				if cfg.IncludeFunding {
					fr := p.funding[i][j]
					if !math.IsNaN(fr) {
						dr -= w * fr
						fundingTotal += w * fr
					}
				}
			}
			for sym, w := range curShorts {
				j := indexOf(p.Symbols, sym)
				if j < 0 {
					continue
				}
				pn := p.close[i][j]
				pp := p.close[prev][j]
				if math.IsNaN(pn) || math.IsNaN(pp) || pp <= 0 {
					continue
				}
				dr += -w * (pn/pp - 1)
				if cfg.IncludeFunding {
					fr := p.funding[i][j]
					if !math.IsNaN(fr) {
						dr += w * fr
						fundingTotal -= w * fr
					}
				}
			}
			eq *= 1 + dr
			dailyRet = append(dailyRet, dr)
			if eq > peak {
				peak = eq
			}
			if dd := eq/peak - 1; dd < maxDD {
				maxDD = dd
			}
		}

		// 2) rebalance check
		if (i-startIdx)%cfg.HoldDays == 0 {
			elig := p.EligibleAt(i, cfg)
			universes = append(universes, len(elig))
			longs, shorts := p.SelectLongShort(i, cfg)
			if longs == nil && shorts == nil {
				equityCurve = append(equityCurve, eq)
				continue
			}
			newLongSet := map[string]struct{}{}
			newShortSet := map[string]struct{}{}
			newLongList := make([]string, 0, len(longs))
			newShortList := make([]string, 0, len(shorts))
			for _, r := range longs {
				newLongSet[r.Symbol] = struct{}{}
				newLongList = append(newLongList, r.Symbol)
			}
			for _, r := range shorts {
				newShortSet[r.Symbol] = struct{}{}
				newShortList = append(newShortList, r.Symbol)
			}

			turnL := symmetricDiffFraction(curLongs, newLongSet, cfg.TopN)
			turnS := symmetricDiffFraction(curShorts, newShortSet, cfg.BotN)
			cost := (turnL + turnS) * feeSlip * 2 * cfg.Leverage
			eq *= 1 - cost
			if len(dailyRet) > 0 {
				dailyRet[len(dailyRet)-1] -= cost
			}

			droppedL := setDiff(curLongs, newLongSet)
			droppedS := setDiff(curShorts, newShortSet)
			addedL := setDiff2(newLongSet, curLongs)
			addedS := setDiff2(newShortSet, curShorts)
			trades = append(trades, Trade{
				Time:          p.Days[i],
				NewLongs:      addedL,
				NewShorts:     addedS,
				DroppedLongs:  droppedL,
				DroppedShorts: droppedS,
				CostFraction:  cost,
			})
			rebalances = append(rebalances, p.Days[i])

			// install new positions
			curLongs = map[string]float64{}
			curShorts = map[string]float64{}
			for _, s := range newLongList {
				curLongs[s] = wLong
			}
			for _, s := range newShortList {
				curShorts[s] = wShort
			}
			turnoverSum += (turnL + turnS) / 2
			turnoverN++
		}
		equityCurve = append(equityCurve, eq)
	}

	if len(dailyRet) == 0 {
		return Result{}, fmt.Errorf("xsmom: no return samples; window too short")
	}

	mean, std := meanStd(dailyRet)
	sharpe := 0.0
	if std > 0 {
		sharpe = mean / std * math.Sqrt(365)
	}
	totalRet := eq/initialEquity - 1
	annRet := math.NaN()
	if eq > 0 {
		annRet = math.Pow(eq/initialEquity, 365.0/float64(len(dailyRet))) - 1
	}
	avgUniv := 0.0
	if len(universes) > 0 {
		s := 0
		for _, u := range universes {
			s += u
		}
		avgUniv = float64(s) / float64(len(universes))
	}
	avgTurnover := 0.0
	if turnoverN > 0 {
		avgTurnover = turnoverSum / float64(turnoverN)
	}
	return Result{
		Days:           len(dailyRet),
		StartDate:      p.Days[startIdx],
		EndDate:        p.Days[endIdx-1],
		StartIdx:       startIdx,
		EndIdx:         endIdx,
		InitialEquity:  initialEquity,
		FinalEquity:    eq,
		TotalReturn:    totalRet,
		AnnReturn:      annRet,
		Sharpe:         sharpe,
		MaxDrawdown:    maxDD,
		NumRebalances:  len(rebalances),
		AvgUniverse:    avgUniv,
		FundingPaidPct: fundingTotal * 100,
		DailyReturns:   dailyRet,
		EquityCurve:    equityCurve,
		RebalanceDates: rebalances,
		SymbolTurnover: avgTurnover,
		Trades:         trades,
	}, nil
}

func indexOf(syms []string, s string) int {
	for i, v := range syms {
		if v == s {
			return i
		}
	}
	return -1
}

func symmetricDiffFraction(cur map[string]float64, next map[string]struct{}, basketSize int) float64 {
	if basketSize == 0 {
		return 0
	}
	curSet := map[string]struct{}{}
	for k := range cur {
		curSet[k] = struct{}{}
	}
	diff := 0
	for k := range curSet {
		if _, ok := next[k]; !ok {
			diff++
		}
	}
	for k := range next {
		if _, ok := curSet[k]; !ok {
			diff++
		}
	}
	return float64(diff) / float64(2*basketSize)
}

func setDiff(a map[string]float64, b map[string]struct{}) []string {
	out := make([]string, 0)
	for k := range a {
		if _, ok := b[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func setDiff2(a map[string]struct{}, b map[string]float64) []string {
	out := make([]string, 0)
	for k := range a {
		if _, ok := b[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func meanStd(xs []float64) (float64, float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	mean := s / float64(len(xs))
	v := 0.0
	for _, x := range xs {
		d := x - mean
		v += d * d
	}
	v /= float64(len(xs))
	return mean, math.Sqrt(v)
}
