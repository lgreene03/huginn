// Package portfolio provides a thread-safe portfolio tracker for paper-trading.
package portfolio

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/lgreene03/huginn/internal/model"
)

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
func (p *Portfolio) ApplyFill(fill model.Fill) {
	p.mu.Lock()
	defer p.mu.Unlock()

	pos, exists := p.positions[fill.Instrument]
	if !exists {
		pos = &Position{Instrument: fill.Instrument}
		p.positions[fill.Instrument] = pos
	}

	cost := fill.FillPrice * fill.Quantity

	// executedCost / executedSlippage track the portion of the fill that
	// actually applied. For a buy that is the whole fill; for a sell that has
	// been clipped to available inventory (long-only), it is the scaled share.
	executedCost := fill.TransactionCost
	executedSlippage := fill.SlippageBps

	switch fill.Side {
	case model.Buy:
		// Cost basis is held INCLUSIVE of the buy-side transaction cost, so
		// realized PnL on a later sell is automatically net of the entry fee
		// (quant-5). AverageCost therefore represents the all-in per-unit cost.
		totalCost := pos.AverageCost*pos.Quantity + cost + fill.TransactionCost
		pos.Quantity += fill.Quantity
		if pos.Quantity > 0 {
			pos.AverageCost = totalCost / pos.Quantity
		}
		p.cash -= cost + fill.TransactionCost

	case model.Sell:
		// Long-only invariant (quant-6): never realize PnL on quantity we do
		// not hold and never drive the position negative (no shorting in this
		// spot sim). Clip an over-sell to available inventory and warn.
		sellQty := fill.Quantity
		if sellQty > pos.Quantity {
			slog.Warn("Sell clipped to available inventory (long-only)",
				"instrument", fill.Instrument,
				"requested_qty", fmt.Sprintf("%.8f", fill.Quantity),
				"available_qty", fmt.Sprintf("%.8f", pos.Quantity),
			)
			sellQty = pos.Quantity
		}

		// Scale the executed economics to the (possibly clipped) quantity so
		// cash, costs and slippage reflect only what actually filled.
		var fillFraction float64
		if fill.Quantity > 0 {
			fillFraction = sellQty / fill.Quantity
		}
		executedCost = fill.TransactionCost * fillFraction
		executedSlippage = fill.SlippageBps * fillFraction
		proceeds := fill.FillPrice * sellQty

		if sellQty > 0 {
			// Realized PnL is NET of both legs' transaction costs: the entry
			// fee is already embedded in AverageCost, and the exit fee
			// (executedCost) is subtracted here (quant-5).
			realized := (fill.FillPrice-pos.AverageCost)*sellQty - executedCost
			p.realizedPnL += realized
		}
		pos.Quantity -= sellQty
		p.cash += proceeds - executedCost

		// Reset average cost if position is flat.
		if pos.Quantity <= 1e-12 {
			pos.Quantity = 0
			pos.AverageCost = 0
		}
	}

	pos.LastMarkPrice = fill.FillPrice
	p.totalFills++
	p.totalCosts += executedCost
	p.totalSlippage += executedSlippage
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
		if pCopy.Quantity > 0 {
			// Mark to last fill price; fall back to cost basis if a position
			// somehow has no mark yet (every fill sets LastMarkPrice).
			mark := pCopy.LastMarkPrice
			if mark <= 0 {
				mark = pCopy.AverageCost
			}
			pCopy.UnrealizedPnL = (mark - pCopy.AverageCost) * pCopy.Quantity
			positionsValue += pCopy.Quantity * mark
		}
		unrealized += pCopy.UnrealizedPnL
		snap.Positions[inst] = pCopy
	}

	snap.UnrealizedPnL = unrealized
	// Total equity = cash + MARKET VALUE of open positions (qty * mark), NOT
	// cash + unrealized. Cash already paid out each position's cost basis on the
	// buy, so adding back only the unrealized PnL drops that cost basis and
	// understates equity by it whenever inventory is open — which silently
	// corrupts TotalValue, the equity curve, drawdown, and every return derived
	// from them. (When flat, positionsValue == 0 and TotalValue == cash.)
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
