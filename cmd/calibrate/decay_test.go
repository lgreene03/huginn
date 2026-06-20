package main

import (
	"math"
	"testing"
)

func TestSharpeDecayRatioCalibrate(t *testing.T) {
	if got := sharpeDecayRatio(2.0, 1.0); math.Abs(got-0.5) > 1e-9 {
		t.Errorf("decay = %f, want 0.5", got)
	}
	if got := sharpeDecayRatio(0, 1.0); !math.IsNaN(got) {
		t.Errorf("decay = %f, want NaN for IS≤0", got)
	}
}

func TestMeanStdCalibrate(t *testing.T) {
	mean, std := meanStd([]float64{2, 4, 6})
	if math.Abs(mean-4) > 1e-9 {
		t.Errorf("mean = %f, want 4", mean)
	}
	// population std of {2,4,6} = sqrt(8/3) ≈ 1.633
	if math.Abs(std-math.Sqrt(8.0/3.0)) > 1e-9 {
		t.Errorf("std = %f, want %f", std, math.Sqrt(8.0/3.0))
	}
}
