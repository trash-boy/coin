package arbitrage

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

const (
	DirectionLongSpotShortPerp = "LONG_SPOT_SHORT_PERP"
	DirectionLongPerpShortSpot = "LONG_PERP_SHORT_SPOT"
)

type FundingOpportunity struct {
	Symbol         string
	MarkPrice      float64
	IndexPrice     float64
	FundingRate    float64
	AnnualizedRate float64
	BasisRate      float64
	Direction      string
}

type BasisOpportunity struct {
	Symbol       string
	SpotEntry    float64
	FuturesEntry float64
	GrossEdge    float64
	CostRate     float64
	NetEdge      float64
	MaxBaseQty   float64
	Direction    string
}

func BuildFundingOpportunity(symbol string, markPrice, indexPrice, fundingRate, minAbsFundingRate float64) (FundingOpportunity, bool) {
	if markPrice <= 0 || indexPrice <= 0 {
		return FundingOpportunity{}, false
	}
	if math.Abs(fundingRate) < minAbsFundingRate {
		return FundingOpportunity{}, false
	}
	direction := DirectionLongSpotShortPerp
	if fundingRate < 0 {
		direction = DirectionLongPerpShortSpot
	}
	return FundingOpportunity{
		Symbol:         strings.ToUpper(symbol),
		MarkPrice:      markPrice,
		IndexPrice:     indexPrice,
		FundingRate:    fundingRate,
		AnnualizedRate: fundingRate * 3 * 365,
		BasisRate:      markPrice/indexPrice - 1,
		Direction:      direction,
	}, true
}

func BuildBasisOpportunity(symbol string, spotBid, spotBidQty, spotAsk, spotAskQty, futuresBid, futuresBidQty, futuresAsk, futuresAskQty, costRate, minNetEdge float64) (BasisOpportunity, bool) {
	if spotBid <= 0 || spotAsk <= 0 || futuresBid <= 0 || futuresAsk <= 0 {
		return BasisOpportunity{}, false
	}
	if costRate < 0 {
		costRate = 0
	}
	longSpotGross := futuresBid/spotAsk - 1
	longSpot := BasisOpportunity{
		Symbol:       strings.ToUpper(symbol),
		SpotEntry:    spotAsk,
		FuturesEntry: futuresBid,
		GrossEdge:    longSpotGross,
		CostRate:     costRate,
		NetEdge:      longSpotGross - costRate,
		MaxBaseQty:   minPositive(spotAskQty, futuresBidQty),
		Direction:    DirectionLongSpotShortPerp,
	}

	longPerpGross := spotBid/futuresAsk - 1
	longPerp := BasisOpportunity{
		Symbol:       strings.ToUpper(symbol),
		SpotEntry:    spotBid,
		FuturesEntry: futuresAsk,
		GrossEdge:    longPerpGross,
		CostRate:     costRate,
		NetEdge:      longPerpGross - costRate,
		MaxBaseQty:   minPositive(spotBidQty, futuresAskQty),
		Direction:    DirectionLongPerpShortSpot,
	}

	best := longSpot
	if longPerp.NetEdge > best.NetEdge {
		best = longPerp
	}
	if best.NetEdge < minNetEdge {
		return BasisOpportunity{}, false
	}
	return best, true
}

func SortFunding(in []FundingOpportunity) {
	sort.Slice(in, func(i, j int) bool {
		return math.Abs(in[i].AnnualizedRate) > math.Abs(in[j].AnnualizedRate)
	})
}

func SortBasis(in []BasisOpportunity) {
	sort.Slice(in, func(i, j int) bool {
		return in[i].NetEdge > in[j].NetEdge
	})
}

func ValidateSymbols(symbols []string) error {
	if len(symbols) == 0 {
		return fmt.Errorf("at least one symbol is required")
	}
	for _, symbol := range symbols {
		if strings.TrimSpace(symbol) == "" {
			return fmt.Errorf("symbol cannot be empty")
		}
	}
	return nil
}

func minPositive(a, b float64) float64 {
	if a <= 0 {
		return b
	}
	if b <= 0 {
		return a
	}
	if a < b {
		return a
	}
	return b
}
