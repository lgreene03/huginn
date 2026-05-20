import { useState, useEffect, useRef } from 'react';
import './App.css';

interface Position {
  Instrument: string;
  Quantity: number;
  AverageCost: number;
  UnrealizedPnL: number;
  LastMarkPrice: number;
}

interface PortfolioSnapshot {
  Timestamp: string;
  Cash: number;
  Positions: { [key: string]: Position };
  RealizedPnL: number;
  UnrealizedPnL: number;
  TotalValue: number;
  TotalFills: number;
  TotalCosts: number;
}

interface Fill {
  OrderID: string;
  Instrument: string;
  Side: number; // 0 = BUY, 1 = SELL
  Quantity: number;
  FillPrice: number;
  TransactionCost: number;
  SlippageBps: number;
  Timestamp: string;
}

const API_BASE = 'http://localhost:8083';

function App() {
  const [connected, setConnected] = useState(false);
  const [portfolio, setPortfolio] = useState<PortfolioSnapshot | null>(null);
  const [halted, setHalted] = useState(false);
  const [fills, setFills] = useState<Fill[]>([]);
  const [equityHistory, setEquityHistory] = useState<{ time: string; value: number }[]>([]);

  // Console execution state
  const [instrument, setInstrument] = useState('BTC-USD');
  const [side, setSide] = useState<'BUY' | 'SELL'>('BUY');
  const [quantity, setQuantity] = useState('0.50000000');
  const [price, setPrice] = useState('65250.00');
  const [submitStatus, setSubmitStatus] = useState<{ type: 'success' | 'error'; message: string } | null>(null);
  const [isSubmitting, setIsSubmitting] = useState(false);

  const scrollRef = useRef<HTMLDivElement>(null);

  // Auto-scroll executions log on new entries
  useEffect(() => {
    if (scrollRef.current) {
      scrollRef.current.scrollTop = 0; // Keep newest at the top
    }
  }, [fills]);

  // Establish SSE Connection
  useEffect(() => {
    const sseUrl = `${API_BASE}/api/stream`;
    console.log(`Connecting to SSE stream: ${sseUrl}`);
    const eventSource = new EventSource(sseUrl);

    eventSource.onopen = () => {
      setConnected(true);
      setSubmitStatus(null);
    };

    eventSource.onmessage = (event) => {
      try {
        const payload = JSON.parse(event.data);
        setPortfolio(payload.portfolio);
        setHalted(payload.halted);
        setFills(payload.fills || []);

        if (payload.portfolio) {
          const rawTime = payload.portfolio.Timestamp || payload.timestamp;
          const timeStr = new Date(rawTime).toLocaleTimeString([], {
            hour: '2-digit',
            minute: '2-digit',
            second: '2-digit',
          });

          setEquityHistory((prev) => {
            // Avoid adding identical contiguous timestamps
            if (prev.length > 0 && prev[prev.length - 1].time === timeStr) {
              return prev;
            }
            const next = [...prev, { time: timeStr, value: payload.portfolio.TotalValue }];
            if (next.length > 50) {
              return next.slice(next.length - 50);
            }
            return next;
          });
        }
      } catch (err) {
        console.error('Failed to parse SSE event data', err);
      }
    };

    eventSource.onerror = (err) => {
      console.error('SSE Connection Error:', err);
      setConnected(false);
      eventSource.close();
    };

    return () => {
      eventSource.close();
    };
  }, []);

  // Trigger manual breaker halt
  const handleHalt = async () => {
    try {
      const res = await fetch(`${API_BASE}/api/breaker/trigger`, {
        method: 'POST',
      });
      if (res.ok) {
        setHalted(true);
        setSubmitStatus({
          type: 'error',
          message: 'Strategy execution manually HALTED. All new prospective executions are blocked.',
        });
      }
    } catch (err) {
      console.error('Failed to trigger circuit breaker', err);
    }
  };

  // Reset breaker halt
  const handleResume = async () => {
    try {
      const res = await fetch(`${API_BASE}/api/breaker/reset`, {
        method: 'POST',
      });
      if (res.ok) {
        setHalted(false);
        setSubmitStatus({
          type: 'success',
          message: 'Strategy execution successfully RESUMED. Risk controls online.',
        });
      }
    } catch (err) {
      console.error('Failed to reset circuit breaker', err);
    }
  };

  // Submit manual simulated fill
  const handleExecuteTrade = async (e: React.FormEvent) => {
    e.preventDefault();
    setIsSubmitting(true);
    setSubmitStatus(null);

    const qtyVal = parseFloat(quantity);
    const pxVal = parseFloat(price);

    if (isNaN(qtyVal) || qtyVal <= 0 || isNaN(pxVal) || pxVal <= 0) {
      setSubmitStatus({
        type: 'error',
        message: 'Invalid trade quantity or price parameters.',
      });
      setIsSubmitting(false);
      return;
    }

    try {
      const res = await fetch(`${API_BASE}/api/fills/mock`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
        },
        body: JSON.stringify({
          instrument,
          side,
          quantity: qtyVal,
          price: pxVal,
        }),
      });

      const data = await res.json();

      if (res.status === 200) {
        setSubmitStatus({
          type: 'success',
          message: `Simulated fill SUCCESS: ${side} ${qtyVal} ${instrument} @ $${pxVal.toLocaleString(undefined, { minimumFractionDigits: 2 })}`,
        });
      } else if (res.status === 403) {
        setSubmitStatus({
          type: 'error',
          message: 'Simulated fill REJECTED: Manual Circuit Breaker is ACTIVE.',
        });
      } else {
        setSubmitStatus({
          type: 'error',
          message: `Execution failed: ${data.message || 'Unknown risk or engine error.'}`,
        });
      }
    } catch {
      setSubmitStatus({
        type: 'error',
        message: 'Network error. Failed to reach the strategy engine api.',
      });
    } finally {
      setIsSubmitting(false);
    }
  };

  // Format currencies beautifully
  const formatCurrency = (val: number | undefined) => {
    if (val === undefined) return '$0.00';
    return new Intl.NumberFormat('en-US', {
      style: 'currency',
      currency: 'USD',
      minimumFractionDigits: 2,
    }).format(val);
  };

  // Position processing
  const activePositions = portfolio && portfolio.Positions
    ? Object.values(portfolio.Positions).filter(p => Math.abs(p.Quantity) > 1e-8)
    : [];

  // PnL totals
  const realizedPnL = portfolio?.RealizedPnL || 0.0;
  const unrealizedPnL = portfolio?.UnrealizedPnL || 0.0;
  const totalPnL = realizedPnL + unrealizedPnL;

  // Render SVG Path for Equity Curve
  const renderEquityChart = () => {
    if (equityHistory.length < 2) {
      return (
        <div className="empty-state">
          <svg width="24" height="24" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24">
            <path strokeLinecap="round" strokeLinejoin="round" d="M2.25 18L9 11.25l4.306 4.307a11.95 11.95 0 015.814-5.519l2.74-1.22m0 0l-5.94-2.28m5.94 2.28l-2.28 5.941" />
          </svg>
          Waiting for live telemetry updates...
        </div>
      );
    }

    const width = 600;
    const height = 200;
    const padding = 20;
    const chartWidth = width - padding * 2;
    const chartHeight = height - padding * 2;

    const values = equityHistory.map((h) => h.value);
    const minVal = Math.min(...values);
    const maxVal = Math.max(...values);
    const valueRange = maxVal - minVal;
    
    // Buffer margins to look professional
    const yMin = valueRange === 0 ? minVal - 100 : minVal - valueRange * 0.1;
    const yMax = valueRange === 0 ? maxVal + 100 : maxVal + valueRange * 0.1;
    const yRange = yMax - yMin;

    const getX = (index: number) => {
      return padding + (index / (equityHistory.length - 1)) * chartWidth;
    };

    const getY = (val: number) => {
      return padding + chartHeight - ((val - yMin) / yRange) * chartHeight;
    };

    // Construct SVG path points
    const points = equityHistory.map((pt, i) => `${getX(i)},${getY(pt.value)}`);
    const linePath = `M ${points.join(' L ')}`;
    
    // Gradient area path closes at the bottom
    const areaPath = `${linePath} L ${getX(equityHistory.length - 1)},${height - padding} L ${getX(0)},${height - padding} Z`;

    // Gridlines positions
    const gridLinesCount = 3;
    const gridYVals = Array.from({ length: gridLinesCount + 1 }, (_, i) => yMin + (i * yRange) / gridLinesCount);

    return (
      <svg viewBox={`0 0 ${width} ${height}`} className="chart-svg">
        <defs>
          <linearGradient id="chart-gradient" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stopColor="var(--neon-cyan)" stopOpacity="0.35" />
            <stop offset="100%" stopColor="var(--neon-cyan)" stopOpacity="0.0" />
          </linearGradient>
        </defs>

        {/* Gridlines */}
        {gridYVals.map((val, idx) => {
          const y = getY(val);
          return (
            <g key={idx}>
              <line x1={padding} y1={y} x2={width - padding} y2={y} className="chart-grid" />
              <text x={padding + 4} y={y - 4} className="chart-text">
                {formatCurrency(val)}
              </text>
            </g>
          );
        })}

        {/* Shadow Gradient Area */}
        <path d={areaPath} className="chart-area" />

        {/* Main Line */}
        <path d={linePath} className="chart-line" />

        {/* Latest Value Pulsing Glow Dot */}
        <circle
          cx={getX(equityHistory.length - 1)}
          cy={getY(equityHistory[equityHistory.length - 1].value)}
          r="4"
          fill="var(--neon-cyan)"
          filter="drop-shadow(0 0 6px var(--neon-cyan))"
        />
      </svg>
    );
  };

  return (
    <>
      <header>
        <div className="brand">
          <svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="var(--neon-cyan)" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round">
            <path d="M12 2L2 7l10 5 10-5-10-5z" />
            <path d="M2 17l10 5 10-5" />
            <path d="M2 12l10 5 10-5" />
          </svg>
          <h1>HUGINN</h1>
          <span>ADMIN CTRL</span>
        </div>

        <div className="status-indicator">
          <div className={`dot ${connected ? 'dot-connected' : 'dot-disconnected'}`} />
          {connected ? 'ENGINE SSE ONLINE' : 'ENGINE SSE OFFLINE'}
        </div>
      </header>

      <div className="container">
        {/* SECTION 1: MANUAL CIRCUIT BREAKER STATUS */}
        <div className={`banner ${halted ? 'banner-halted' : 'banner-running'}`}>
          <div className="banner-content">
            <svg
              width="32"
              height="32"
              viewBox="0 0 24 24"
              fill="none"
              stroke={halted ? 'var(--neon-red)' : 'var(--neon-green)'}
              strokeWidth="2.5"
              className={halted ? 'pulse-icon' : ''}
            >
              {halted ? (
                <path strokeLinecap="round" strokeLinejoin="round" d="M12 9v2m0 4h.01m-6.938 4h13.856c1.54 0 2.502-1.667 1.732-3L13.732 4c-.77-1.333-2.694-1.333-3.464 0L3.34 16c-.77 1.333.192 3 1.732 3z" />
              ) : (
                <path strokeLinecap="round" strokeLinejoin="round" d="M9 12l2 2 4-4m5.618-4.016A11.955 11.955 0 0112 2.944a11.955 11.955 0 01-8.618 3.04A12.02 12.02 0 003 9c0 5.591 3.824 10.29 9 11.622 5.176-1.332 9-6.03 9-11.622 0-1.042-.133-2.052-.382-3.016z" />
              )}
            </svg>
            <div>
              <div className="banner-title">
                {halted ? 'CRITICAL SYSTEM STATUS: EXECUTION HALTED' : 'SYSTEM STATUS: OPERATIONAL & RUNNING'}
              </div>
              <div className="banner-desc">
                {halted
                  ? 'Manual Circuit Breaker is ACTIVE. Strategy signals are evaluated, but prospective executions are instantly rejected.'
                  : 'Engine is actively monitoring real-time Order Book Imbalance features and executing quantitative strategy signals.'}
              </div>
            </div>
          </div>

          <div>
            {halted ? (
              <button className="btn btn-resume" onClick={handleResume}>
                <svg width="18" height="18" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24">
                  <path strokeLinecap="round" strokeLinejoin="round" d="M9 12l2 2 4-4m6 2a9 9 0 11-18 0 9 9 0 0118 0z" />
                </svg>
                RESET CIRCUIT BREAKER
              </button>
            ) : (
              <button className="btn btn-halt" onClick={handleHalt}>
                <svg width="18" height="18" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24">
                  <path strokeLinecap="round" strokeLinejoin="round" d="M18.364 18.364A9 9 0 005.636 5.636m12.728 12.728A9 9 0 015.636 5.636m12.728 12.728L5.636 5.636" />
                </svg>
                TRIGGER MANUAL HALT
              </button>
            )}
          </div>
        </div>

        {/* SECTION 2: PORTFOLIO telemetry STATS */}
        <div className="stats-grid">
          <div className="panel">
            <div className="stat-label">Total Equity Value</div>
            <div className="stat-val stat-val-cyan">{formatCurrency(portfolio?.TotalValue)}</div>
            <div className="stat-sub">
              <span>Paper-Trading Balance</span>
              <span className="stat-sub-val">Uptime: 100%</span>
            </div>
          </div>

          <div className="panel">
            <div className="stat-label">Cash Reserve</div>
            <div className="stat-val">{formatCurrency(portfolio?.Cash)}</div>
            <div className="stat-sub">
              <span>Liquid Assets</span>
              <span className="stat-sub-val">Slippage: Dynamic</span>
            </div>
          </div>

          <div className="panel">
            <div className="stat-label">Real-time Strategy PnL</div>
            <div className={`stat-val ${totalPnL >= 0 ? 'stat-val-green' : 'stat-val-red'}`}>
              {totalPnL >= 0 ? '+' : ''}{formatCurrency(totalPnL)}
            </div>
            <div className="stat-sub">
              <span>Realized: {formatCurrency(realizedPnL)}</span>
              <span>Unrealized: {formatCurrency(unrealizedPnL)}</span>
            </div>
          </div>

          <div className="panel">
            <div className="stat-label">Session Telemetry</div>
            <div className="stat-val text-mono" style={{ color: 'var(--neon-orange)' }}>
              {portfolio?.TotalFills || 0}
            </div>
            <div className="stat-sub">
              <span>Executed Fills</span>
              <span className="stat-sub-val">Fees paid: {formatCurrency(portfolio?.TotalCosts)}</span>
            </div>
          </div>
        </div>

        {/* SECTION 3: EQUITY GRAPH & MANUAL FORM */}
        <div className="main-grid">
          <div className="panel">
            <h2 style={{ fontSize: '1.2rem', fontWeight: 600, borderBottom: '1px solid rgba(255,255,255,0.06)', paddingBottom: '0.5rem' }}>
              Real-time Portfolio Equity Curve
            </h2>
            <div className="chart-container">{renderEquityChart()}</div>
          </div>

          <div className="panel">
            <h2 style={{ fontSize: '1.2rem', fontWeight: 600, borderBottom: '1px solid rgba(255,255,255,0.06)', paddingBottom: '0.5rem', marginBottom: '1rem' }}>
              Manual Execution Console
            </h2>
            <form onSubmit={handleExecuteTrade}>
              <div className="form-group">
                <label>Instrument Asset</label>
                <select className="input-field" value={instrument} onChange={(e) => setInstrument(e.target.value)}>
                  <option value="BTC-USD">BTC-USD (Bitcoin / US Dollar)</option>
                  <option value="ETH-USD">ETH-USD (Ethereum / US Dollar)</option>
                  <option value="SOL-USD">SOL-USD (Solana / US Dollar)</option>
                  <option value="XRP-USD">XRP-USD (Ripple / US Dollar)</option>
                </select>
              </div>

              <div className="form-row">
                <div className="form-group">
                  <label>Order Side</label>
                  <select className="input-field" value={side} onChange={(e) => setSide(e.target.value as 'BUY' | 'SELL')}>
                    <option value="BUY">BUY / LONG</option>
                    <option value="SELL">SELL / SHORT</option>
                  </select>
                </div>
                <div className="form-group">
                  <label>Order Size (Qty)</label>
                  <input
                    type="number"
                    step="0.00000001"
                    min="0.00000001"
                    className="input-field input-field-mono"
                    value={quantity}
                    onChange={(e) => setQuantity(e.target.value)}
                    required
                  />
                </div>
              </div>

              <div className="form-group">
                <label>Simulated Price ($)</label>
                <input
                  type="number"
                  step="0.01"
                  min="0.01"
                  className="input-field input-field-mono"
                  value={price}
                  onChange={(e) => setPrice(e.target.value)}
                  required
                />
              </div>

              <button type="submit" className="btn btn-submit" disabled={isSubmitting || !connected}>
                {isSubmitting ? 'ROUTING ORDER TO PORTFOLIO...' : 'SUBMIT SIMULATED ORDER'}
              </button>
            </form>

            {submitStatus && (
              <div className={`alert-box ${submitStatus.type === 'success' ? 'alert-success' : 'alert-error'}`}>
                <svg width="18" height="18" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24">
                  {submitStatus.type === 'success' ? (
                    <path strokeLinecap="round" strokeLinejoin="round" d="M9 12l2 2 4-4m6 2a9 9 0 11-18 0 9 9 0 0118 0z" />
                  ) : (
                    <path strokeLinecap="round" strokeLinejoin="round" d="M12 8v4m0 4h.01M21 12a9 9 0 11-18 0 9 9 0 0118 0z" />
                  )}
                </svg>
                {submitStatus.message}
              </div>
            )}
          </div>
        </div>

        {/* SECTION 4: POSITIONS TABLE & EXECUTIONS TICKER */}
        <div className="main-grid">
          <div className="panel">
            <h2 style={{ fontSize: '1.2rem', fontWeight: 600, borderBottom: '1px solid rgba(255,255,255,0.06)', paddingBottom: '0.5rem', marginBottom: '0.5rem' }}>
              Active Portfolio Positions
            </h2>
            <div className="table-wrapper">
              {activePositions.length === 0 ? (
                <div className="empty-state">
                  <svg width="24" height="24" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24">
                    <path strokeLinecap="round" strokeLinejoin="round" d="M20 7l-8-4-8 4m16 0l-8 4m8-4v10l-8 4m0-10L4 7m8 4v10M4 7v10l8 4" />
                  </svg>
                  No active exposure. Portfolio is completely flat.
                </div>
              ) : (
                <table>
                  <thead>
                    <tr>
                      <th>Instrument</th>
                      <th>Direction</th>
                      <th className="text-right">Quantity</th>
                      <th className="text-right">Avg Entry Cost</th>
                      <th className="text-right">Last Mark Price</th>
                      <th className="text-right">Unrealized PnL</th>
                    </tr>
                  </thead>
                  <tbody>
                    {activePositions.map((pos) => {
                      const isLong = pos.Quantity > 0;
                      return (
                        <tr key={pos.Instrument}>
                          <td style={{ fontWeight: 600 }}>{pos.Instrument}</td>
                          <td>
                            <span className={`badge ${isLong ? 'badge-buy' : 'badge-sell'}`}>
                              {isLong ? 'LONG' : 'SHORT'}
                            </span>
                          </td>
                          <td className="text-mono text-right" style={{ fontWeight: 500 }}>
                            {pos.Quantity.toFixed(8)}
                          </td>
                          <td className="text-mono text-right">{formatCurrency(pos.AverageCost)}</td>
                          <td className="text-mono text-right">{formatCurrency(pos.LastMarkPrice)}</td>
                          <td className={`text-mono text-right ${pos.UnrealizedPnL >= 0 ? 'stat-val-green' : 'stat-val-red'}`} style={{ fontWeight: 600 }}>
                            {pos.UnrealizedPnL >= 0 ? '+' : ''}{formatCurrency(pos.UnrealizedPnL)}
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              )}
            </div>
          </div>

          <div className="panel">
            <h2 style={{ fontSize: '1.2rem', fontWeight: 600, borderBottom: '1px solid rgba(255,255,255,0.06)', paddingBottom: '0.5rem' }}>
              Execution Fills Log
            </h2>
            <div className="scroll-container" ref={scrollRef}>
              {fills.length === 0 ? (
                <div className="empty-state">
                  <svg width="24" height="24" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24">
                    <path strokeLinecap="round" strokeLinejoin="round" d="M9 5H7a2 2 0 00-2 2v12a2 2 0 002 2h10a2 2 0 002-2V7a2 2 0 00-2-2h-2M9 5a2 2 0 002 2h2a2 2 0 002-2M9 5a2 2 0 012-2h2a2 2 0 012 2" />
                  </svg>
                  No trade executions in active session.
                </div>
              ) : (
                fills.map((fill) => {
                  const isBuy = fill.Side === 0;
                  const tradeTime = new Date(fill.Timestamp).toLocaleTimeString([], {
                    hour: '2-digit',
                    minute: '2-digit',
                    second: '2-digit',
                  });
                  return (
                    <div key={fill.OrderID} className="ticker-item">
                      <div className="ticker-header">
                        <span className="ticker-inst">{fill.Instrument}</span>
                        <span className="ticker-time">{tradeTime}</span>
                      </div>
                      <div className="ticker-details">
                        <div>
                          <span className={`badge ${isBuy ? 'badge-buy' : 'badge-sell'}`} style={{ marginRight: '0.5rem' }}>
                            {isBuy ? 'BUY' : 'SELL'}
                          </span>
                          <span className="ticker-qty-price">
                            {fill.Quantity.toFixed(4)} @ {formatCurrency(fill.FillPrice)}
                          </span>
                        </div>
                        <span className="ticker-cost">
                          Fee: {formatCurrency(fill.TransactionCost)}
                        </span>
                      </div>
                    </div>
                  );
                })
              )}
            </div>
          </div>
        </div>
      </div>
    </>
  );
}

export default App;
