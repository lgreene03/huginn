package metrics

import (
	"math"
)

// CalculateSharpeRatio computes the annualized Sharpe ratio from a series of equity curve points.
func CalculateSharpeRatio(equity []float64, riskFreeRate float64) float64 {
	if len(equity) < 2 {
		return 0.0
	}

	var returns []float64
	for i := 1; i < len(equity); i++ {
		ret := (equity[i] - equity[i-1]) / equity[i-1]
		returns = append(returns, ret)
	}

	var sum float64
	for _, r := range returns {
		sum += r
	}
	mean := sum / float64(len(returns))

	var variance float64
	for _, r := range returns {
		variance += math.Pow(r-mean, 2)
	}
	stdDev := math.Sqrt(variance / float64(len(returns)))

	if stdDev == 0 {
		return 0.0
	}

	// Assuming daily returns, annualized factor is sqrt(252)
	annualizedReturn := mean * 252
	annualizedStdDev := stdDev * math.Sqrt(252)

	return (annualizedReturn - riskFreeRate) / annualizedStdDev
}

// CalculateMaxDrawdown computes the maximum percentage drop from a peak in the equity curve.
func CalculateMaxDrawdown(equity []float64) float64 {
	var maxDrawdown float64
	var peak float64

	for _, value := range equity {
		if value > peak {
			peak = value
		}
		
		if peak > 0 {
			drawdown := (peak - value) / peak
			if drawdown > maxDrawdown {
				maxDrawdown = drawdown
			}
		}
	}

	return maxDrawdown
}
