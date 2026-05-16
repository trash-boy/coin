package arbitrage

import "testing"

func TestBuildFundingOpportunityDirections(t *testing.T) {
	pos, ok := BuildFundingOpportunity("btcusdt", 101, 100, 0.0002, 0.0001)
	if !ok {
		t.Fatal("expected positive funding opportunity")
	}
	if pos.Symbol != "BTCUSDT" || pos.Direction != DirectionLongSpotShortPerp {
		t.Fatalf("unexpected positive funding direction: %+v", pos)
	}
	if abs(pos.AnnualizedRate-0.0002*3*365) > 1e-12 {
		t.Fatalf("unexpected annualized funding: %f", pos.AnnualizedRate)
	}
	if abs(pos.BasisRate-0.01) > 1e-12 {
		t.Fatalf("unexpected basis: %f", pos.BasisRate)
	}

	neg, ok := BuildFundingOpportunity("ethusdt", 99, 100, -0.0002, 0.0001)
	if !ok {
		t.Fatal("expected negative funding opportunity")
	}
	if neg.Direction != DirectionLongPerpShortSpot {
		t.Fatalf("unexpected negative funding direction: %+v", neg)
	}
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func TestBuildFundingOpportunityThreshold(t *testing.T) {
	_, ok := BuildFundingOpportunity("BTCUSDT", 100, 100, 0.00005, 0.0001)
	if ok {
		t.Fatal("expected funding below threshold to be filtered")
	}
}

func TestBuildBasisOpportunityChoosesBestNetEdge(t *testing.T) {
	opp, ok := BuildBasisOpportunity(
		"btcusdt",
		100, 2, 101, 3,
		103, 4, 104, 5,
		0.001, 0.0001,
	)
	if !ok {
		t.Fatal("expected basis opportunity")
	}
	if opp.Symbol != "BTCUSDT" || opp.Direction != DirectionLongSpotShortPerp {
		t.Fatalf("unexpected basis direction: %+v", opp)
	}
	if opp.MaxBaseQty != 3 {
		t.Fatalf("expected max base qty from spot ask/futures bid, got %f", opp.MaxBaseQty)
	}
	if opp.NetEdge <= 0 {
		t.Fatalf("expected positive net edge, got %f", opp.NetEdge)
	}
}

func TestBuildBasisOpportunityFiltersAfterCost(t *testing.T) {
	_, ok := BuildBasisOpportunity(
		"BTCUSDT",
		100, 1, 100.1, 1,
		100.2, 1, 100.3, 1,
		0.002, 0.0001,
	)
	if ok {
		t.Fatal("expected basis opportunity to be filtered after costs")
	}
}
