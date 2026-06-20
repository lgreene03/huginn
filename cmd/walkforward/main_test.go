package main

import (
	"math"
	"testing"

	"github.com/lgreene03/huginn/internal/config"
)

func TestParseFloatsOr(t *testing.T) {
	cases := []struct {
		in   string
		dflt float64
		want []float64
	}{
		{"", 0.7, []float64{0.7}},
		{"  ", 0.7, []float64{0.7}},
		{"0.5,0.6,0.7", 0.7, []float64{0.5, 0.6, 0.7}},
		{"0.5, 0.6 ,0.7", 0.7, []float64{0.5, 0.6, 0.7}},
		{"junk", 0.7, []float64{0.7}}, // all unparseable → fall back to default
		{"0.5,junk,0.7", 0.7, []float64{0.5, 0.7}},
	}
	for _, c := range cases {
		got := parseFloatsOr(c.in, c.dflt)
		if len(got) != len(c.want) {
			t.Fatalf("parseFloatsOr(%q): len %d, want %d (%v)", c.in, len(got), len(c.want), got)
		}
		for i := range got {
			if math.Abs(got[i]-c.want[i]) > 1e-9 {
				t.Errorf("parseFloatsOr(%q)[%d] = %f, want %f", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestBuildGridCartesian(t *testing.T) {
	cfg := &config.Config{}
	cfg.Strategy.Threshold = 0.7
	cfg.Strategy.OrderSize = 0.01

	// Default (no flags) → single combo, i.e. no real selection.
	if g := buildGrid(cfg, "", ""); len(g) != 1 {
		t.Errorf("default grid len = %d, want 1", len(g))
	}

	// 3 thresholds × 2 order sizes = 6 combos.
	g := buildGrid(cfg, "0.5,0.6,0.7", "0.01,0.02")
	if len(g) != 6 {
		t.Errorf("grid len = %d, want 6", len(g))
	}
}

func TestSharpeDecayRatio(t *testing.T) {
	if got := sharpeDecayRatio(2.0, 1.0); math.Abs(got-0.5) > 1e-9 {
		t.Errorf("decay = %f, want 0.5", got)
	}
	// IS Sharpe ≤ 0 → undefined (NaN), no edge to decay from.
	if got := sharpeDecayRatio(0, 1.0); !math.IsNaN(got) {
		t.Errorf("decay = %f, want NaN for IS≤0", got)
	}
	if got := sharpeDecayRatio(-1, 1.0); !math.IsNaN(got) {
		t.Errorf("decay = %f, want NaN for IS<0", got)
	}
}

func TestMeanStd(t *testing.T) {
	mean, std := meanStd([]float64{1, 2, 3})
	if math.Abs(mean-2) > 1e-9 {
		t.Errorf("mean = %f, want 2", mean)
	}
	// population std of {1,2,3} = sqrt(2/3) ≈ 0.8165
	if math.Abs(std-math.Sqrt(2.0/3.0)) > 1e-9 {
		t.Errorf("std = %f, want %f", std, math.Sqrt(2.0/3.0))
	}
	if m, s := meanStd(nil); m != 0 || s != 0 {
		t.Errorf("meanStd(nil) = (%f,%f), want (0,0)", m, s)
	}
}
