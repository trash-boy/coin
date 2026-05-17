package strategy

import (
	"fmt"
	"time"
)

type Candle struct {
	OpenTime  time.Time
	CloseTime time.Time
	Open      float64
	High      float64
	Low       float64
	Close     float64
	Volume    float64
}

type Side string

const (
	Flat  Side = "FLAT"
	Long  Side = "LONG"
	Short Side = "SHORT"
)

type Action string

const (
	ActionHold  Action = "HOLD"
	ActionEnter Action = "ENTER"
	ActionExit  Action = "EXIT"
	ActionNone  Action = "NO_TRADE"
)

type MarginMode string

const (
	MarginIsolated MarginMode = "isolated"
	MarginCross    MarginMode = "cross"
)

type Config struct {
	FastEMA   int
	SlowEMA   int
	TrendEMA  int
	ATRPeriod int
	TrailATR  float64

	RiskPerTrade   float64
	MaxLeverage    float64
	MaxMarginUse   float64
	MaxDrawdown    float64
	DailyLossLimit float64
	CooldownBars   int
	MarginMode     MarginMode

	HigherTFHours         int
	MajorTFHours          int
	HigherFastEMA         int
	HigherSlowEMA         int
	ADXPeriod             int
	MinADXLong            float64
	MinADXShort           float64
	EfficiencyPeriod      int
	MinEfficiency         float64
	PullbackBars          int
	MaxEntryATRDistance   float64
	SwingLookback         int
	StructureBufferATR    float64
	MinStopATR            float64
	MaxStopATR            float64
	BreakEvenR            float64
	TrailActivationR      float64
	PyramidATR            float64
	MaxAdds               int
	MinAddSpacingBars     int
	PyramidRiskScale      float64
	LongRiskMultiplier    float64
	ShortRiskMultiplier   float64
	VolTargetATRRatio     float64
	MinEffectiveLeverage  float64
	LiquidationStopBuffer float64
	FallbackMMR           float64
	DDReduce1             float64
	DDReduce2             float64
	DDScale1              float64
	DDScale2              float64
	KellyLookback         int
	KellyFraction         float64
	MinKellyScale         float64
	MaxKellyScale         float64
	FundingBlockLong      float64
	FundingBlockShort     float64
	FundingRiskCut        float64
	MaintenanceBrackets   []MaintenanceBracket

	MinATRRatio float64
	MaxATRRatio float64
	FeeRate     float64
	SlippageBps float64

	// --- Aggressive 15m mode ---
	Mode             string  // "" = leverage_trend default; "aggressive_15m" = 15m breakout
	BreakoutLookback int     // Donchian length, e.g. 20
	VolumeMultiplier float64 // volume >= avg * this, e.g. 1.5
	EntryATRStop     float64 // initial stop = entry +/- this * ATR, e.g. 1.5
}

func DefaultConfig() Config {
	return Config{
		FastEMA:               20,
		SlowEMA:               60,
		TrendEMA:              200,
		ATRPeriod:             14,
		TrailATR:              3.0,
		RiskPerTrade:          0.008,
		MaxLeverage:           2.0,
		MaxMarginUse:          0.80,
		MaxDrawdown:           0.15,
		DailyLossLimit:        0.03,
		CooldownBars:          6,
		MarginMode:            MarginIsolated,
		HigherTFHours:         4,
		MajorTFHours:          24,
		HigherFastEMA:         20,
		HigherSlowEMA:         50,
		ADXPeriod:             14,
		MinADXLong:            20,
		MinADXShort:           24,
		EfficiencyPeriod:      24,
		MinEfficiency:         0.25,
		PullbackBars:          8,
		MaxEntryATRDistance:   1.2,
		SwingLookback:         12,
		StructureBufferATR:    0.25,
		MinStopATR:            1.2,
		MaxStopATR:            3.2,
		BreakEvenR:            1.5,
		TrailActivationR:      2.0,
		PyramidATR:            1.5,
		MaxAdds:               2,
		MinAddSpacingBars:     4,
		PyramidRiskScale:      0.45,
		LongRiskMultiplier:    1.0,
		ShortRiskMultiplier:   0.65,
		VolTargetATRRatio:     0.010,
		MinEffectiveLeverage:  0.40,
		LiquidationStopBuffer: 0.70,
		FallbackMMR:           0.01,
		DDReduce1:             0.05,
		DDReduce2:             0.10,
		DDScale1:              0.70,
		DDScale2:              0.40,
		KellyLookback:         30,
		KellyFraction:         0.25,
		MinKellyScale:         0.35,
		MaxKellyScale:         1.20,
		FundingBlockLong:      0.00015,
		FundingBlockShort:     0.00012,
		FundingRiskCut:        0.00008,
		MinATRRatio:           0.002,
		MaxATRRatio:           0.08,
		FeeRate:               0.0004,
		SlippageBps:           2.0,
		Mode:                  "",
		BreakoutLookback:      20,
		VolumeMultiplier:      1.5,
		EntryATRStop:          1.5,
	}
}

func (c Config) Validate() error {
	if c.FastEMA <= 1 || c.SlowEMA <= c.FastEMA || c.TrendEMA <= c.SlowEMA {
		return fmt.Errorf("EMA periods must satisfy 1 < fast < slow < trend")
	}
	if c.ATRPeriod <= 1 {
		return fmt.Errorf("ATR period must be greater than 1")
	}
	if c.TrailATR <= 0 {
		return fmt.Errorf("trailing ATR multiple must be positive")
	}
	if c.RiskPerTrade <= 0 || c.RiskPerTrade > 0.05 {
		return fmt.Errorf("risk per trade must be in (0, 0.05]")
	}
	if c.MaxLeverage < 1 || c.MaxLeverage > 10 {
		return fmt.Errorf("max leverage must be in [1, 10] for this strategy")
	}
	if c.MaxMarginUse <= 0 || c.MaxMarginUse > 1 {
		return fmt.Errorf("max margin use must be in (0, 1]")
	}
	if c.MaxDrawdown <= 0 || c.MaxDrawdown > 0.50 {
		return fmt.Errorf("max drawdown must be in (0, 0.50]")
	}
	if c.DailyLossLimit <= 0 || c.DailyLossLimit > c.MaxDrawdown {
		return fmt.Errorf("daily loss limit must be positive and no larger than max drawdown")
	}
	if c.MarginMode != MarginIsolated && c.MarginMode != MarginCross {
		return fmt.Errorf("margin mode must be isolated or cross")
	}
	if c.MinATRRatio < 0 || c.MaxATRRatio <= c.MinATRRatio {
		return fmt.Errorf("invalid ATR ratio bounds")
	}
	if c.HigherTFHours <= 0 || c.MajorTFHours <= 0 || c.MajorTFHours < c.HigherTFHours {
		return fmt.Errorf("higher timeframe hours must satisfy 0 < higher <= major")
	}
	if c.HigherFastEMA <= 1 || c.HigherSlowEMA <= c.HigherFastEMA {
		return fmt.Errorf("higher timeframe EMAs must satisfy 1 < fast < slow")
	}
	if c.ADXPeriod <= 1 || c.EfficiencyPeriod <= 1 || c.PullbackBars <= 0 || c.SwingLookback <= 1 {
		return fmt.Errorf("market-state and pullback periods must be positive")
	}
	if c.MinADXLong < 0 || c.MinADXShort < 0 || c.MinEfficiency < 0 || c.MinEfficiency > 1 {
		return fmt.Errorf("invalid market-state thresholds")
	}
	if c.MaxEntryATRDistance <= 0 || c.StructureBufferATR < 0 || c.MinStopATR <= 0 || c.MaxStopATR < c.MinStopATR {
		return fmt.Errorf("invalid entry or structure stop settings")
	}
	if c.BreakEvenR < 0 || c.TrailActivationR < 0 || c.PyramidATR <= 0 || c.MaxAdds < 0 || c.MinAddSpacingBars < 0 || c.PyramidRiskScale < 0 {
		return fmt.Errorf("invalid trailing or pyramid settings")
	}
	if c.LongRiskMultiplier <= 0 || c.ShortRiskMultiplier <= 0 {
		return fmt.Errorf("risk multipliers must be positive")
	}
	if c.VolTargetATRRatio <= 0 || c.MinEffectiveLeverage <= 0 || c.MinEffectiveLeverage > c.MaxLeverage {
		return fmt.Errorf("invalid volatility targeting settings")
	}
	if c.LiquidationStopBuffer <= 0 || c.LiquidationStopBuffer >= 1 {
		return fmt.Errorf("liquidation stop buffer must be in (0, 1)")
	}
	if c.FallbackMMR <= 0 || c.FallbackMMR >= 1 {
		return fmt.Errorf("fallback maintenance margin ratio must be in (0, 1)")
	}
	if c.DDReduce1 <= 0 || c.DDReduce2 < c.DDReduce1 || c.DDScale1 <= 0 || c.DDScale2 <= 0 || c.DDScale2 > c.DDScale1 {
		return fmt.Errorf("invalid drawdown scaling settings")
	}
	if c.KellyLookback <= 0 || c.KellyFraction < 0 || c.MinKellyScale <= 0 || c.MaxKellyScale < c.MinKellyScale {
		return fmt.Errorf("invalid Kelly scaling settings")
	}
	if c.FeeRate < 0 || c.SlippageBps < 0 {
		return fmt.Errorf("fee and slippage cannot be negative")
	}
	return nil
}

type Signal struct {
	Time       time.Time
	Action     Action
	Side       Side
	Price      float64
	Quantity   float64
	Notional   float64
	Leverage   float64
	StopLoss   float64
	TakeProfit float64
	Reason     string
}

type FundingRate struct {
	Time      time.Time
	Rate      float64
	MarkPrice float64
}

type MaintenanceBracket struct {
	NotionalFloor    float64
	NotionalCap      float64
	MaintMarginRatio float64
	Cum              float64
	InitialLeverage  int
}
