// Package portfolio provides a thread-safe portfolio tracker for paper-trading.
package portfolio

import (
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/lgreene03/huginn/internal/model"
)

// sign returns +1 for positive, -1 for negative, 0 for zero.
func sign(x float64) float64 {
	switch {
	case x > 0:
		return 1
	case x < 0:
		return -1
	default:
		return 0
	}
}

// sameSign reports whether a and b are both strictly positive or both strictly
// negative (a fill adds to a position in the same direction).
func sameSign(a, b float64) bool {
	return (a > 0 && b > 0) || (a < 0 && b < 0)
}

// Portfolio maintains the current state of a simulated trading account.
// All methods are safe for concurrent access.
type Portfolio struct {
	mu sync.RWMutex

	cash          float64
	positions     map[string]*Position // instrument -> position
	realizedPnL   float64
	totalFills    int
	totalCosts    float64
	totalSlippage float64
	fills         []model.Fill
}

// Position tracks a single instrument's holdings.
type Position struct {
	Instrument    string
	Quantity      float64
	AverageCost   float64
	UnrealizedPnL float64
	LastMarkPrice float64
}

// Snapshot is a point-in-time, immutable view of the portfolio for telemetry.
type Snapshot struct {
	Timestamp     time.Time
	Cash          float64
	Positions     map[string]Position
	RealizedPnL   float64
	UnrealizedPnL float64
	TotalValue    float64
	TotalFills    int
	TotalCosts    float64
}

// New creates a portfolio with the given initial cash balance.
func New(initialCash float64) *Portfolio {
	return &Portfolio{
		cash:      initialCash,
		positions: make(map[string]*Position),
	}
}

// ApplyFill records a simulated execution against the portfolio.
//
// Positions are SIGNED: Quantity > 0 is long, < 0 is short, 0 is flat.
// AverageCost is the POSITIVE average entry price of the currently-open
// position. The fill economics generalize the original long-only avg-cost
// ledger: a same-direction fill adds at a fee-inclusive weighted average, an
// opposing fill closes (realizing PnL net of the prorated closing fee) and may
// flip through zero, opening a fresh position in the opposite direction at the
// fill price (with the remaining fee portion embedded in the new cost basis).
// A long-only fill sequence produces results identical to the prior code.
func (p *Portfolio) ApplyFill(fill model.Fill) {
	p.mu.Lock()
	defer p.mu.Unlock()

	pos, exists := p.positions[fill.Instrument]
	if !exists {
		pos = &Position{Instrument: fill.Instrument}
		p.positions[fill.Instrument] = pos
	}

	// signed is the signed quantity delta of this fill: +qty for a BUY,
	// -qty for a SELL. fillQty is always the positive traded size.
	fillQty := fill.Quantity
	signed := fillQty
	if fill.Side == model.Sell {
		signed = -fillQty
	}
	fee := fill.TransactionCost

	// Cash: a BUY pays notional + fee; a SELL receives notional, minus fee.
	// -signed*price covers both: BUY (signed>0) debits, SELL (signed<0) credits.
	p.cash += -signed*fill.FillPrice - fee

	oldQty := pos.Quantity
	avg := pos.AverageCost

	switch {
	case oldQty == 0 || sameSign(oldQty, signed):
		// Opening, or adding in the SAME direction: fee-inclusive weighted
		// average over the absolute sizes. The fee adjusts the basis in the
		// position's OWN direction: a long pays price+fee (basis up), a short
		// receives price−fee (effective entry down), so realized PnL on the
		// eventual close is net of BOTH legs' fees for longs AND shorts.
		newQty := oldQty + signed
		avg = (avg*math.Abs(oldQty) + fill.FillPrice*fillQty + sign(newQty)*fee) / math.Abs(newQty)
		pos.Quantity = newQty
		pos.AverageCost = avg

	default:
		// Fill OPPOSES the position: it closes up to abs(oldQty), and any
		// remainder flips through zero to open a new position.
		closeQty := math.Min(math.Abs(oldQty), fillQty)
		// Realized PnL on the closed portion, net of the prorated closing fee.
		// sign(oldQty) = +1 closing a long (profit when price>avg),
		//               -1 closing a short (profit when price<avg).
		realized := sign(oldQty)*(fill.FillPrice-avg)*closeQty - fee*(closeQty/fillQty)
		p.realizedPnL += realized

		pos.Quantity = oldQty + signed
		if math.Abs(pos.Quantity) < 1e-12 {
			pos.Quantity = 0
			pos.AverageCost = 0
		} else if remainder := fillQty - closeQty; remainder > 0 {
			// Flipped through zero: the remainder opens a NEW position at the
			// fill price, with the still-unallocated fee portion embedded in the
			// new position's own direction (raises a long's basis, lowers a
			// short's) so the eventual close nets both fees correctly.
			pos.AverageCost = fill.FillPrice + sign(pos.Quantity)*(fee*(remainder/fillQty))/remainder
		}
		// (No flip: position merely reduced; AverageCost of the surviving
		// side is unchanged.)
	}

	pos.LastMarkPrice = fill.FillPrice
	p.totalFills++
	p.totalCosts += fee
	p.totalSlippage += fill.SlippageBps
	p.fills = append(p.fills, fill)

	// Demoted from Info → Debug in Phase 3: at production event rates this
	// line fires hundreds of times per minute and drowns the log. The fill
	// is already durably journaled and surfaced via metrics + the SSE
	// stream; LOG_LEVEL=debug brings it back when an operator wants it.
	slog.Debug("Fill applied",
		"instrument", fill.Instrument,
		"side", fill.Side.String(),
		"quantity", fmt.Sprintf("%.8f", fill.Quantity),
		"fill_price", fmt.Sprintf("%.2f", fill.FillPrice),
		"tx_cost", fmt.Sprintf("%.4f", fill.TransactionCost),
		"cash_remaining", fmt.Sprintf("%.2f", p.cash),
		"position_qty", fmt.Sprintf("%.8f", pos.Quantity),
		"realized_pnl", fmt.Sprintf("%.4f", p.realizedPnL),
	)
}

// Snapshot returns a point-in-time, immutable copy of the portfolio state.
func (p *Portfolio) Snapshot() Snapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()

	snap := Snapshot{
		Timestamp:   time.Now(),
		Cash:        p.cash,
		Positions:   make(map[string]Position, len(p.positions)),
		RealizedPnL: p.realizedPnL,
		TotalFills:  p.totalFills,
		TotalCosts:  p.totalCosts,
	}

	var unrealized float64
	var positionsValue float64
	for inst, pos := range p.positions {
		pCopy := *pos
		if pCopy.Quantity != 0 {
			// Mark to last fill price; fall back to cost basis if a position
			// somehow has no mark yet (every fill sets LastMarkPrice).
			mark := pCopy.LastMarkPrice
			if mark <= 0 {
				mark = pCopy.AverageCost
			}
			// Signed: a short (Quantity<0) has positive unrealized PnL as the
			// mark falls below AverageCost, and contributes a negative market
			// value (a liability) to equity.
			pCopy.UnrealizedPnL = (mark - pCopy.AverageCost) * pCopy.Quantity
			positionsValue += pCopy.Quantity * mark
		}
		unrealized += pCopy.UnrealizedPnL
		snap.Positions[inst] = pCopy
	}

	snap.UnrealizedPnL = unrealized
	// Total equity = cash + MARKET VALUE of open positions (signed qty * mark),
	// NOT cash + unrealized. Cash already paid out each long position's cost
	// basis on the buy (and took in a short's proceeds), so adding back only the
	// unrealized PnL would drop that basis and corrupt TotalValue, the equity
	// curve, drawdown, and every return derived from them. A short Quantity is
	// negative, so it contributes negatively (a liability). When flat,
	// positionsValue == 0 and TotalValue == cash.
	snap.TotalValue = snap.Cash + positionsValue

	return snap
}

// Fills returns a copy of all simulated fills.
func (p *Portfolio) Fills() []model.Fill {
	p.mu.RLock()
	defer p.mu.RUnlock()

	fillsCopy := make([]model.Fill, len(p.fills))
	copy(fillsCopy, p.fills)
	return fillsCopy
}
