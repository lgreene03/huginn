package metrics

import (
	"math"
	"testing"
)

func TestCalculateSharpeRatio(t *testing.T) {
	equity := []float64{100.0, 101.0, 102.01, 103.0301} // 1% steady growth
	sharpe := CalculateSharpeRatio(equity, 0.0)
	
	if sharpe <= 0 || math.IsNaN(sharpe) {
		t.Errorf("Expected positive sharpe ratio, got %v", sharpe)
	}
}

func TestCalculateMaxDrawdown(t *testing.T) {
	equity := []float64{100.0, 110.0, 99.0, 120.0} 
	// Peak is 110, trough is 99. Drop is 11. 11/110 = 0.10 (10%)
	mdd := CalculateMaxDrawdown(equity)
	
	if math.Abs(mdd-0.10) > 1e-6 {
		t.Errorf("Expected max drawdown to be 0.10, got %v", mdd)
	}
}
