package main

import (
	"fmt"
	"sort"
	"strings"
)

// RenderSVG produces a dependency-free, committable SVG with two side-by-side
// panels:
//
//   - LEFT: NET Sharpe vs transaction-cost-bps. One polyline per cost_hurdle_k
//     column, so the cost sensitivity of each gate setting is visible at a
//     glance. The break-even cost (where net PnL crosses zero on the baseline
//     column) is drawn as a vertical marker.
//   - RIGHT: turnover vs NET Sharpe frontier — a scatter of every grid point,
//     showing how harder gating trades turnover for risk-adjusted return.
//
// The SVG is hand-written (no plotting library) so the output is a small,
// diff-friendly, version-controllable artifact.
func RenderSVG(strategyName string, res SweepResult) string {
	const (
		w        = 920
		h        = 420
		padL     = 60
		padR     = 24
		padT     = 56
		padB     = 48
		panelGap = 40
	)
	panelW := (w - padL - padR - panelGap) / 2
	plotH := h - padT - padB

	leftX0 := padL
	rightX0 := padL + panelW + panelGap

	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %d %d" xmlns="http://www.w3.org/2000/svg" font-family="ui-sans-serif,system-ui,sans-serif">`, w, h)
	b.WriteString(`<rect width="100%" height="100%" fill="#0f1419"/>`)
	fmt.Fprintf(&b, `<text x="%d" y="28" fill="#e6e6e6" font-size="18" font-weight="600">Cost Sweep — %s</text>`, padL, escape(strategyName))

	// Conclusions subtitle.
	concl := fmt.Sprintf("best cost_hurdle_k=%.2f (NetSharpe=%.3f)", res.BestHurdleK, res.BestHurdleNetSharpe)
	if res.BreakEvenFound {
		concl += fmt.Sprintf("  •  break-even cost=%.2f bps", res.BreakEvenCost)
	} else {
		concl += "  •  break-even: none in range"
	}
	fmt.Fprintf(&b, `<text x="%d" y="46" fill="#8b98a5" font-size="12">%s</text>`, padL, escape(concl))

	// ── LEFT panel: NetSharpe vs cost ──────────────────────────────────────
	renderAxes(&b, leftX0, padT, panelW, plotH, "transaction-cost (bps)", "Net Sharpe")
	cols := groupByHurdle(res.Points)
	costLo, costHi := costRange(res.Points)
	shLo, shHi := sharpeRange(res.Points)
	palette := []string{"#4ea1ff", "#ffb24e", "#4eff9b", "#ff6b9d", "#b88bff", "#ffe14e"}

	for i, k := range sortedKeys(cols) {
		pts := cols[k]
		sort.Slice(pts, func(a, c int) bool { return pts[a].TxCostBps < pts[c].TxCostBps })
		color := palette[i%len(palette)]
		var poly strings.Builder
		for _, p := range pts {
			px := mapX(p.TxCostBps, costLo, costHi, leftX0, panelW)
			py := mapY(p.NetSharpe, shLo, shHi, padT, plotH)
			fmt.Fprintf(&poly, "%.1f,%.1f ", px, py)
			fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="2.5" fill="%s"/>`, px, py, color)
		}
		fmt.Fprintf(&b, `<polyline points="%s" fill="none" stroke="%s" stroke-width="1.8"/>`, strings.TrimSpace(poly.String()), color)
		// Legend entry.
		ly := padT + 14 + i*16
		fmt.Fprintf(&b, `<rect x="%d" y="%d" width="10" height="10" fill="%s"/>`, leftX0+8, ly-9, color)
		fmt.Fprintf(&b, `<text x="%d" y="%d" fill="#c8d0d8" font-size="11">K=%.2f</text>`, leftX0+22, ly, k)
	}

	// Zero line (Net Sharpe = 0) if in range.
	if shLo <= 0 && shHi >= 0 {
		zy := mapY(0, shLo, shHi, padT, plotH)
		fmt.Fprintf(&b, `<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" stroke="#3a4552" stroke-width="1" stroke-dasharray="3,3"/>`,
			leftX0, zy, leftX0+panelW, zy)
	}
	// Break-even cost marker.
	if res.BreakEvenFound && res.BreakEvenCost >= costLo && res.BreakEvenCost <= costHi {
		bx := mapX(res.BreakEvenCost, costLo, costHi, leftX0, panelW)
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%d" x2="%.1f" y2="%d" stroke="#ff5252" stroke-width="1.4" stroke-dasharray="5,3"/>`,
			bx, padT, bx, padT+plotH)
		fmt.Fprintf(&b, `<text x="%.1f" y="%d" fill="#ff7b7b" font-size="10" text-anchor="middle">break-even %.2f</text>`,
			bx, padT+plotH+14, res.BreakEvenCost)
	}
	axisLabels(&b, leftX0, padT, panelW, plotH, costLo, costHi, shLo, shHi)

	// ── RIGHT panel: turnover vs NetSharpe frontier ────────────────────────
	renderAxes(&b, rightX0, padT, panelW, plotH, "Net Sharpe", "turnover (x)")
	nsLo, nsHi := sharpeRange(res.Points)
	toLo, toHi := turnoverRange(res.Points)
	for i, k := range sortedKeys(cols) {
		color := palette[i%len(palette)]
		for _, p := range cols[k] {
			px := mapX(p.NetSharpe, nsLo, nsHi, rightX0, panelW)
			py := mapY(p.Turnover, toLo, toHi, padT, plotH)
			fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="3" fill="%s" fill-opacity="0.85"/>`, px, py, color)
		}
	}
	axisLabels(&b, rightX0, padT, panelW, plotH, nsLo, nsHi, toLo, toHi)

	b.WriteString(`</svg>`)
	return b.String()
}

// renderAxes draws the bounding box and axis titles for a panel.
func renderAxes(b *strings.Builder, x0, y0, pw, ph int, xTitle, yTitle string) {
	fmt.Fprintf(b, `<rect x="%d" y="%d" width="%d" height="%d" fill="#11161c" stroke="#2a323c" stroke-width="1"/>`, x0, y0, pw, ph)
	fmt.Fprintf(b, `<text x="%d" y="%d" fill="#8b98a5" font-size="11" text-anchor="middle">%s</text>`, x0+pw/2, y0+ph+34, escape(xTitle))
	// Rotated y-axis title.
	fmt.Fprintf(b, `<text x="%d" y="%d" fill="#8b98a5" font-size="11" text-anchor="middle" transform="rotate(-90 %d %d)">%s</text>`,
		x0-42, y0+ph/2, x0-42, y0+ph/2, escape(yTitle))
}

// axisLabels writes min/max tick labels at the corners of a panel.
func axisLabels(b *strings.Builder, x0, y0, pw, ph int, xLo, xHi, yLo, yHi float64) {
	fmt.Fprintf(b, `<text x="%d" y="%d" fill="#5d6b78" font-size="10">%.2f</text>`, x0+2, y0+ph+14, xLo)
	fmt.Fprintf(b, `<text x="%d" y="%d" fill="#5d6b78" font-size="10" text-anchor="end">%.2f</text>`, x0+pw-2, y0+ph+14, xHi)
	fmt.Fprintf(b, `<text x="%d" y="%d" fill="#5d6b78" font-size="10">%.2f</text>`, x0+2, y0+10, yHi)
	fmt.Fprintf(b, `<text x="%d" y="%d" fill="#5d6b78" font-size="10">%.2f</text>`, x0+2, y0+ph-2, yLo)
}

func mapX(v, lo, hi float64, x0, pw int) float64 {
	if hi == lo {
		return float64(x0) + float64(pw)/2
	}
	return float64(x0) + (v-lo)/(hi-lo)*float64(pw)
}

func mapY(v, lo, hi float64, y0, ph int) float64 {
	if hi == lo {
		return float64(y0) + float64(ph)/2
	}
	// SVG y grows downward; high value sits at the top.
	return float64(y0) + (1-(v-lo)/(hi-lo))*float64(ph)
}

func groupByHurdle(pts []SweepPoint) map[float64][]SweepPoint {
	m := make(map[float64][]SweepPoint)
	for _, p := range pts {
		m[p.HurdleK] = append(m[p.HurdleK], p)
	}
	return m
}

func sortedKeys(m map[float64][]SweepPoint) []float64 {
	keys := make([]float64, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Float64s(keys)
	return keys
}

// padRange widens a degenerate [lo,hi] (lo==hi) to a unit band so the axis
// mapping never divides by zero and a single-valued series is still visible.
func padRange(lo, hi float64) (float64, float64) {
	if hi > lo {
		return lo, hi
	}
	if lo == 0 {
		return -1, 1
	}
	d := absf(lo) * 0.1
	return lo - d, lo + d
}

func costRange(pts []SweepPoint) (float64, float64) {
	lo, hi := pts[0].TxCostBps, pts[0].TxCostBps
	for _, p := range pts {
		lo = minf(lo, p.TxCostBps)
		hi = maxf(hi, p.TxCostBps)
	}
	return padRange(lo, hi)
}

func sharpeRange(pts []SweepPoint) (float64, float64) {
	lo, hi := pts[0].NetSharpe, pts[0].NetSharpe
	for _, p := range pts {
		lo = minf(lo, p.NetSharpe)
		hi = maxf(hi, p.NetSharpe)
	}
	return padRange(lo, hi)
}

func turnoverRange(pts []SweepPoint) (float64, float64) {
	lo, hi := pts[0].Turnover, pts[0].Turnover
	for _, p := range pts {
		lo = minf(lo, p.Turnover)
		hi = maxf(hi, p.Turnover)
	}
	return padRange(lo, hi)
}

func minf(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func absf(a float64) float64 {
	if a < 0 {
		return -a
	}
	return a
}

// escape minimally escapes text for inclusion in SVG element bodies.
func escape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}
