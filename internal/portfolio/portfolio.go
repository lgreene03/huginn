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

	switch fill.Side {
	case model.Buy:
		// Update average cost
		totalCost := pos.AverageCost*pos.Quantity + cost
		pos.Quantity += fill.Quantity
		if pos.Quantity > 0 {
			pos.AverageCost = totalCost / pos.Quantity
		}
		p.cash -= cost + fill.TransactionCost

	case model.Sell:
		if pos.Quantity > 0 {
			realized := (fill.FillPrice - pos.AverageCost) * fill.Quantity
			p.realizedPnL += realized
		}
		pos.Quantity -= fill.Quantity
		p.cash += cost - fill.TransactionCost

		// Reset average cost if position is flat
		if pos.Quantity <= 1e-12 {
			pos.Quantity = 0
			pos.AverageCost = 0
		}
	}

	pos.LastMarkPrice = fill.FillPrice
	p.totalFills++
	p.totalCosts += fill.TransactionCost
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
	for inst, pos := range p.positions {
		pCopy := *pos
		if pCopy.Quantity > 0 && pCopy.LastMarkPrice > 0 {
			pCopy.UnrealizedPnL = (pCopy.LastMarkPrice - pCopy.AverageCost) * pCopy.Quantity
		}
		unrealized += pCopy.UnrealizedPnL
		snap.Positions[inst] = pCopy
	}

	snap.UnrealizedPnL = unrealized
	snap.TotalValue = snap.Cash + unrealized

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
