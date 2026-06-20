package strategy

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/lgreene03/huginn/internal/model"
)

// OBIThreshold is a mean-reversion strategy driven by Order Book Imbalance,
// filtered by multi-timeframe momentum, volatility, market sentiment,
// funding rate, open interest cascade detection, ML confidence, and news.
//
// # Signal layers (entry)
//
//  1. OBI — primary trigger. Fires when |OBI| > threshold (adaptive via ML).
//  2. Volume spike — blocks trades during abnormal volume (>3x).
//  3. ML confidence — blocks low-confidence signals when model is trained.
//  4. Multi-timeframe momentum — 1m/5m/15m must not conflict with trade direction.
//  5. Sentiment — Fear & Greed Index. Extreme values block contrarian signals.
//  6. News sentiment — LLM-classified headline sentiment blocks conflicting trades.
//  7. Funding rate — extreme funding blocks trades aligned with overleveraged side.
//  8. OI cascade — large OI drops indicate liquidation cascades; block all entries.
//  9. Position throttle — per-instrument (1 position max) and notional exposure cap.
//
// # Risk management (exit)
//
//  - Stop-loss: closes position if price moves against entry by stopLossPct.
//  - Take-profit: closes position if price moves in favour by takeProfitPct.
//  - Time-based exit: closes position after maxHoldTime.
//  - Cooldown: prevents re-entry on the same instrument for cooldown duration.
type OBIThreshold struct {
	mu        sync.Mutex
	Threshold float64
	OrderSize float64

	// maxPosition is the legacy quantity-based cap (kept for API compat).
	// maxNotional is the dollar-based total exposure cap across all instruments.
	maxPosition float64
	maxNotional float64
	netPosition float64

	positions     map[string]*positionEntry
	lastTradeTime map[string]time.Time

	stopLossPct   float64
	takeProfitPct float64
	maxHoldTime   time.Duration
	cooldown      time.Duration
}

type positionEntry struct {
	EntryPrice float64
	EntryTime  time.Time
	Qty        float64
	Side       model.Side
}

func NewOBIThreshold(threshold, orderSize, maxPosition float64) *OBIThreshold {
	return &OBIThreshold{
		Threshold:     threshold,
		OrderSize:     orderSize,
		maxPosition:   maxPosition,
		maxNotional:   500.0, // $500 max total notional exposure across all instruments
		positions:     make(map[string]*positionEntry),
		lastTradeTime: make(map[string]time.Time),
		stopLossPct:   0.005,
		takeProfitPct: 0.003,
		maxHoldTime:   30 * time.Minute,
		cooldown:      60 * time.Second,
	}
}

func (s *OBIThreshold) Name() string {
	return fmt.Sprintf("OBIThreshold(%.2f)", s.Threshold)
}

// totalNotional returns the total dollar exposure across all open positions.
func (s *OBIThreshold) totalNotional() float64 {
	var total float64
	for _, pos := range s.positions {
		total += pos.Qty * pos.EntryPrice
	}
	return total
}

func (s *OBIThreshold) OnFeature(event model.FeatureEvent) []model.Order {
	obi, ok := event.Values["obi"]
	if !ok {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	midPrice := event.Values["midPrice"]
	momentum := event.Values["momentum"]
	momentum1m := event.Values["momentum1m"]
	momentum15m := event.Values["momentum15m"]
	volatility := event.Values["volatility"]
	fearGreed := event.Values["fearGreed"]
	volumeRatio := event.Values["volumeRatio"]
	mlScore := event.Values["mlScore"]
	mlReady := event.Values["mlReady"]
	newsSentiment := event.Values["newsSentiment"]
	fundingRate := event.Values["fundingRate"]
	oiChange := event.Values["oiChange"]

	instrument := event.Instrument
	var orders []model.Order

	// ── Exit logic ──────────────────────────────────────────────────────
	if pos, ok := s.positions[instrument]; ok && midPrice > 0 {
		var exitReason string

		elapsed := event.EventTime.Sub(pos.EntryTime)
		pctMove := (midPrice - pos.EntryPrice) / pos.EntryPrice
		if pos.Side == model.Sell {
			pctMove = -pctMove
		}

		if pctMove <= -s.stopLossPct {
			exitReason = fmt.Sprintf(
				"STOP-LOSS: %.2f%% against entry (limit %.2f%%)",
				pctMove*100, -s.stopLossPct*100,
			)
		} else if pctMove >= s.takeProfitPct {
			exitReason = fmt.Sprintf(
				"TAKE-PROFIT: %.2f%% in favour (target %.2f%%)",
				pctMove*100, s.takeProfitPct*100,
			)
		} else if elapsed >= s.maxHoldTime {
			exitReason = fmt.Sprintf(
				"TIME-EXIT: held %s (limit %s), P&L %.2f%%",
				elapsed.Round(time.Second), s.maxHoldTime, pctMove*100,
			)
		}

		if exitReason != "" {
			closeSide := model.Buy
			if pos.Side == model.Buy {
				closeSide = model.Sell
			}

			orders = append(orders, model.Order{
				Instrument: instrument,
				Side:       closeSide,
				Quantity:   pos.Qty,
				LimitPrice: midPrice,
				Reason: fmt.Sprintf(
					"%s | entry=%.2f now=%.2f held=%s",
					exitReason, pos.EntryPrice, midPrice, elapsed.Round(time.Second),
				),
				Timestamp: event.EventTime,
			})

			if closeSide == model.Buy {
				s.netPosition += pos.Qty
			} else {
				s.netPosition -= pos.Qty
			}

			slog.Info("Position exit",
				"strategy", s.Name(),
				"action", closeSide.String(),
				"instrument", instrument,
				"reason", exitReason,
				"pnl_pct", fmt.Sprintf("%.4f", pctMove),
			)

			delete(s.positions, instrument)
			s.lastTradeTime[instrument] = event.EventTime
			return orders
		}
	}

	// ── Cooldown ────────────────────────────────────────────────────────
	if lastTrade, ok := s.lastTradeTime[instrument]; ok {
		if event.EventTime.Sub(lastTrade) < s.cooldown {
			return nil
		}
	}

	// ── Per-instrument throttle: only 1 open position per instrument ───
	if _, hasPos := s.positions[instrument]; hasPos {
		return nil
	}

	// ── Entry filters ───────────────────────────────────────────────────

	// Adaptive threshold: ML confidence modulates selectivity
	effectiveThreshold := s.Threshold
	if mlReady > 0 {
		if mlScore > 0.7 {
			effectiveThreshold = s.Threshold - 0.02
		} else if mlScore < 0.4 {
			effectiveThreshold = s.Threshold + 0.02
		}
	}

	// Widen threshold in high-volatility regimes
	if volatility > 0.015 {
		effectiveThreshold += 0.05
	}

	// Regime-aware threshold: adapt to detected market microstructure
	regimeHurst := event.Values["regimeHurst"]
	regimeAutocorr := event.Values["regimeAutocorr"]
	if regimeHurst > 0.6 {
		// Trending regime: relax threshold for mean-reversion, reversals are rarer
		effectiveThreshold += 0.03
	} else if regimeAutocorr < -0.2 {
		// Strong mean-reversion regime: tighten threshold, reversals are reliable
		effectiveThreshold -= 0.02
	}

	// OI cascade: large OI drop (>5%) signals liquidation cascade
	if oiChange < -0.05 {
		slog.Debug("Liquidation cascade detected, blocking entry",
			"instrument", instrument,
			"oiChange", fmt.Sprintf("%.2f%%", oiChange*100),
		)
		return nil
	}

	// Volume spike filter
	if volumeRatio > 3.0 {
		return nil
	}

	// ML filter: block low-confidence signals when model is trained
	if mlReady > 0 && mlScore < 0.35 {
		return nil
	}

	// Notional exposure check: would this trade push us over the cap?
	proposedNotional := s.OrderSize * midPrice
	if midPrice > 0 && s.totalNotional()+proposedNotional > s.maxNotional {
		slog.Debug("Notional cap reached",
			"instrument", instrument,
			"current", fmt.Sprintf("$%.2f", s.totalNotional()),
			"proposed", fmt.Sprintf("$%.2f", proposedNotional),
			"cap", fmt.Sprintf("$%.2f", s.maxNotional),
		)
		return nil
	}

	if obi > effectiveThreshold && s.netPosition > -s.maxPosition {
		// ── SELL entry (extreme buy pressure → mean-reversion sell) ──

		// Multi-timeframe momentum: block if ANY timeframe strongly bullish
		if momentum > 0.002 || momentum15m > 0.003 {
			slog.Debug("Sell blocked by multi-TF bullish momentum",
				"m5", fmt.Sprintf("%.4f", momentum),
				"m15", fmt.Sprintf("%.4f", momentum15m),
			)
			return nil
		}

		// Funding rate contrarian filter
		if fundingRate < -0.0003 {
			slog.Debug("Sell blocked by negative funding (shorts crowded)",
				"fundingRate", fmt.Sprintf("%.6f", fundingRate),
			)
			return nil
		}

		if fearGreed > 0 && fearGreed < 20 {
			return nil
		}
		if newsSentiment > 0.4 {
			return nil
		}

		confidence := "normal"
		if momentum1m < -0.001 {
			confidence = "high"
		}

		notional := s.OrderSize * midPrice

		orders = append(orders, model.Order{
			Instrument: instrument,
			Side:       model.Sell,
			Quantity:   s.OrderSize,
			LimitPrice: midPrice,
			Reason: fmt.Sprintf(
				"OBI=%.4f>%.2f m1/5/15=%.4f/%.4f/%.4f fr=%.6f oi=%.1f%% f&g=%.0f ml=%.2f news=%+.2f $%.0f [%s] — sell",
				obi, effectiveThreshold, momentum1m, momentum, momentum15m,
				fundingRate, oiChange*100, fearGreed, mlScore, newsSentiment, notional, confidence,
			),
			Timestamp: event.EventTime,
		})
		s.netPosition -= s.OrderSize
		s.lastTradeTime[instrument] = event.EventTime

		if midPrice > 0 {
			s.positions[instrument] = &positionEntry{
				EntryPrice: midPrice,
				EntryTime:  event.EventTime,
				Qty:        s.OrderSize,
				Side:       model.Sell,
			}
		}

		slog.Info("Strategy signal",
			"strategy", s.Name(),
			"action", "SELL",
			"instrument", instrument,
			"obi", fmt.Sprintf("%.4f", obi),
			"threshold", fmt.Sprintf("%.2f", effectiveThreshold),
			"notional", fmt.Sprintf("$%.2f", notional),
			"exposure", fmt.Sprintf("$%.2f/$%.2f", s.totalNotional(), s.maxNotional),
			"confidence", confidence,
		)

	} else if obi < -effectiveThreshold && s.netPosition < s.maxPosition {
		// ── BUY entry (extreme sell pressure → mean-reversion buy) ───

		// Multi-timeframe momentum: block if ANY timeframe strongly bearish
		if momentum < -0.002 || momentum15m < -0.003 {
			slog.Debug("Buy blocked by multi-TF bearish momentum",
				"m5", fmt.Sprintf("%.4f", momentum),
				"m15", fmt.Sprintf("%.4f", momentum15m),
			)
			return nil
		}

		// Funding rate contrarian filter
		if fundingRate > 0.0005 {
			slog.Debug("Buy blocked by high funding (longs crowded)",
				"fundingRate", fmt.Sprintf("%.6f", fundingRate),
			)
			return nil
		}

		if fearGreed > 80 {
			return nil
		}
		if newsSentiment < -0.4 {
			return nil
		}

		confidence := "normal"
		if momentum1m > 0.001 {
			confidence = "high"
		}

		notional := s.OrderSize * midPrice

		orders = append(orders, model.Order{
			Instrument: instrument,
			Side:       model.Buy,
			Quantity:   s.OrderSize,
			LimitPrice: midPrice,
			Reason: fmt.Sprintf(
				"OBI=%.4f<-%.2f m1/5/15=%.4f/%.4f/%.4f fr=%.6f oi=%.1f%% f&g=%.0f ml=%.2f news=%+.2f $%.0f [%s] — buy",
				obi, effectiveThreshold, momentum1m, momentum, momentum15m,
				fundingRate, oiChange*100, fearGreed, mlScore, newsSentiment, notional, confidence,
			),
			Timestamp: event.EventTime,
		})
		s.netPosition += s.OrderSize
		s.lastTradeTime[instrument] = event.EventTime

		if midPrice > 0 {
			s.positions[instrument] = &positionEntry{
				EntryPrice: midPrice,
				EntryTime:  event.EventTime,
				Qty:        s.OrderSize,
				Side:       model.Buy,
			}
		}

		slog.Info("Strategy signal",
			"strategy", s.Name(),
			"action", "BUY",
			"instrument", instrument,
			"obi", fmt.Sprintf("%.4f", obi),
			"threshold", fmt.Sprintf("%.2f", effectiveThreshold),
			"notional", fmt.Sprintf("$%.2f", notional),
			"exposure", fmt.Sprintf("$%.2f/$%.2f", s.totalNotional(), s.maxNotional),
			"confidence", confidence,
		)
	}

	return orders
}

// ── State persistence ───────────────────────────────────────────────────

type positionStateV2 struct {
	EntryPrice float64 `json:"entry_price"`
	EntryTime  string  `json:"entry_time"`
	Qty        float64 `json:"qty"`
	Side       int     `json:"side"`
}

type obiStateV2 struct {
	NetPosition    float64                    `json:"net_position"`
	Positions      map[string]positionStateV2 `json:"positions"`
	LastTradeTimes map[string]string          `json:"last_trade_times"`
}

func (s *OBIThreshold) MarshalState() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	positions := make(map[string]positionStateV2, len(s.positions))
	for inst, pos := range s.positions {
		positions[inst] = positionStateV2{
			EntryPrice: pos.EntryPrice,
			EntryTime:  pos.EntryTime.Format(time.RFC3339Nano),
			Qty:        pos.Qty,
			Side:       int(pos.Side),
		}
	}

	lastTrades := make(map[string]string, len(s.lastTradeTime))
	for inst, t := range s.lastTradeTime {
		lastTrades[inst] = t.Format(time.RFC3339Nano)
	}

	return MarshalEnvelope(2, obiStateV2{
		NetPosition:    s.netPosition,
		Positions:      positions,
		LastTradeTimes: lastTrades,
	})
}

func (s *OBIThreshold) RestoreState(data []byte) error {
	version, fields, err := ParseEnvelope(data)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	switch version {
	case 1:
		var f struct {
			NetPosition float64 `json:"net_position"`
		}
		if err := json.Unmarshal(fields, &f); err != nil {
			return fmt.Errorf("OBIThreshold: failed to unmarshal v1 fields: %w", err)
		}
		s.netPosition = f.NetPosition
		return nil

	case 2:
		var f obiStateV2
		if err := json.Unmarshal(fields, &f); err != nil {
			return fmt.Errorf("OBIThreshold: failed to unmarshal v2 fields: %w", err)
		}
		s.netPosition = f.NetPosition

		s.positions = make(map[string]*positionEntry, len(f.Positions))
		for inst, ps := range f.Positions {
			t, _ := time.Parse(time.RFC3339Nano, ps.EntryTime)
			s.positions[inst] = &positionEntry{
				EntryPrice: ps.EntryPrice,
				EntryTime:  t,
				Qty:        ps.Qty,
				Side:       model.Side(ps.Side),
			}
		}

		s.lastTradeTime = make(map[string]time.Time, len(f.LastTradeTimes))
		for inst, ts := range f.LastTradeTimes {
			t, _ := time.Parse(time.RFC3339Nano, ts)
			s.lastTradeTime[inst] = t
		}
		return nil

	default:
		return fmt.Errorf("%w: OBIThreshold got v%d", ErrStateVersionMismatch, version)
	}
}

// ── Kelly criterion position sizing ─────────────────────────────────────

// KellyFraction computes the optimal fraction of capital to risk per trade
// using the Kelly criterion: f* = (bp - q) / b
// where b = win/loss ratio, p = win rate, q = 1 - p.
// Returns half-Kelly (f*/2) for safety, clamped to [0, 0.25].
func KellyFraction(winRate, avgWin, avgLoss float64) float64 {
	if avgLoss <= 0 || winRate <= 0 || winRate >= 1 {
		return 0
	}
	b := avgWin / avgLoss
	p := winRate
	q := 1 - p
	f := (b*p - q) / b
	// Half-Kelly for safety
	f = f / 2
	if f < 0 {
		return 0
	}
	if f > 0.25 {
		return 0.25
	}
	return math.Round(f*1000) / 1000
}
