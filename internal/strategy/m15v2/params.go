package m15v2

import "errors"

// Params holds all tunable parameters for the m15v2 aggressive trend-breakout strategy.
// Defaults target an aggressive crypto-perp 15m profile.
type Params struct {
	// --- Trend filters (15m) ---
	EMAFast   int // 21
	EMAMid    int // 55
	EMASlow   int // 200
	ADXPeriod int // 14
	ADXMin    float64

	// --- Higher TF confirmation (1h) ---
	HTFEMAFast int // 21 on 1h
	HTFEMASlow int // 50 on 1h
	UseHTF     bool

	// --- Bollinger squeeze ---
	BBPeriod   int     // 20
	BBStdMult  float64 // 2.0
	SqueezeLB  int     // lookback for percentile of BB width, e.g. 120 bars
	SqueezePct float64 // require width <= this percentile of recent N bars (0..1)

	// --- Donchian channel (entry breakout + trail) ---
	DonchianEntry int // 20
	DonchianTrail int // 10 (reverse channel for trailing stop)

	// --- ATR ---
	ATRPeriod int // 14
	ATRStopK  float64

	// --- Pullback limit entry ---
	UsePullback bool
	PullbackEMA int     // EMA used as pullback target (e.g. 21)
	PullbackOff float64 // additional offset in ATR units (e.g. 0.1)
	LimitTTL    int     // bars to keep limit alive before cancelling

	// --- Take-profit / risk ---
	TP1R         float64 // 1.0R
	TP2R         float64 // 2.0R
	TP1PartFrac  float64 // fraction to close at TP1 (e.g. 0.5)
	TP2PartFrac  float64 // fraction to close at TP2 (e.g. 0.3); rest trails
	MaxBarsHold  int     // hard time exit
	UseTimeExit  bool

	// --- Position sizing ---
	RiskPerTrade float64 // 0.015 = 1.5% of equity
	MaxLeverage  float64 // 5x

	// --- Kelly ---
	UseKelly      bool
	KellyLookback int     // 30 trades
	KellyFraction float64 // 0.5 = half-Kelly
	KellyFloor    float64 // min multiplier (e.g. 0.25)
	KellyCap      float64 // max multiplier (e.g. 1.0)

	// --- Daily/streak guards ---
	DailyLossLimit  float64 // 0.03 = 3% of starting-day equity
	LossStreakHalt  int     // 3 consecutive losses
	CooldownMinutes int     // 60min after streak

	// --- Costs ---
	FeeBps      float64 // taker fee bps per side
	SlippageBps float64
}

// DefaultParams returns the aggressive 15m profile.
func DefaultParams() Params {
	return Params{
		EMAFast:   21,
		EMAMid:    55,
		EMASlow:   200,
		ADXPeriod: 14,
		ADXMin:    20,

		HTFEMAFast: 21,
		HTFEMASlow: 50,
		UseHTF:     true,

		BBPeriod:   20,
		BBStdMult:  2.0,
		SqueezeLB:  120,
		SqueezePct: 0.40,

		DonchianEntry: 20,
		DonchianTrail: 10,

		ATRPeriod: 14,
		ATRStopK:  1.5,

		UsePullback: true,
		PullbackEMA: 21,
		PullbackOff: 0.10,
		LimitTTL:    3,

		TP1R:        0.7, // tuned 2026-05 from 180d/8sym backtest
		TP2R:        2.5, // tuned 2026-05 from 180d/8sym backtest
		TP1PartFrac: 0.5,
		TP2PartFrac: 0.3,
		MaxBarsHold: 32, // 8h on 15m, tuned 2026-05
		UseTimeExit: true,

		RiskPerTrade: 0.015,
		MaxLeverage:  5,

		UseKelly:      true,
		KellyLookback: 30,
		KellyFraction: 0.5,
		KellyFloor:    0.25,
		KellyCap:      1.0,

		DailyLossLimit:  0.03,
		LossStreakHalt:  3,
		CooldownMinutes: 60,

		FeeBps:      4,
		SlippageBps: 2,
	}
}

// Validate sanity-checks the parameters.
func (p Params) Validate() error {
	if p.EMAFast <= 0 || p.EMAMid <= p.EMAFast || p.EMASlow <= p.EMAMid {
		return errors.New("EMA periods must satisfy 0 < fast < mid < slow")
	}
	if p.ADXPeriod <= 0 || p.ATRPeriod <= 0 || p.BBPeriod <= 0 {
		return errors.New("indicator periods must be positive")
	}
	if p.DonchianEntry <= 0 || p.DonchianTrail <= 0 {
		return errors.New("donchian periods must be positive")
	}
	if p.RiskPerTrade <= 0 || p.RiskPerTrade > 0.05 {
		return errors.New("RiskPerTrade out of sane range (0,0.05]")
	}
	if p.MaxLeverage <= 0 || p.MaxLeverage > 20 {
		return errors.New("MaxLeverage out of sane range (0,20]")
	}
	if p.TP1R <= 0 || p.TP2R <= p.TP1R {
		return errors.New("TP2R must be > TP1R > 0")
	}
	if p.TP1PartFrac < 0 || p.TP1PartFrac > 1 ||
		p.TP2PartFrac < 0 || p.TP2PartFrac > 1 ||
		p.TP1PartFrac+p.TP2PartFrac > 1 {
		return errors.New("TP partial fractions invalid")
	}
	if p.UseKelly {
		if p.KellyLookback <= 0 || p.KellyFraction <= 0 ||
			p.KellyFloor < 0 || p.KellyCap < p.KellyFloor {
			return errors.New("Kelly parameters invalid")
		}
	}
	return nil
}

// WarmupBars returns the minimum number of bars required before the strategy
// can produce reliable signals.
func (p Params) WarmupBars() int {
	m := p.EMASlow
	if p.SqueezeLB > m {
		m = p.SqueezeLB
	}
	if p.DonchianEntry > m {
		m = p.DonchianEntry
	}
	if p.BBPeriod > m {
		m = p.BBPeriod
	}
	if p.ATRPeriod > m {
		m = p.ATRPeriod
	}
	return m + 5
}
