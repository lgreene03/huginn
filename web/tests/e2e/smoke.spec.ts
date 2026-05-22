import { test, expect } from '@playwright/test';

// Stub responses for all Huginn API calls so no real server is needed.
const stubConfig = {
  strategy_name: 'obi',
  threshold: 0.7,
  order_size: 0.01,
  fast_period: 12,
  slow_period: 26,
  position_limit_hard: 0.5,
};

test.beforeEach(async ({ page }) => {
  // Mock strategy config endpoint.
  await page.route('**/api/strategy/config', (route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(stubConfig) })
  );

  // Mock snapshot history endpoint (empty → chart shows placeholder state).
  await page.route('**/api/snapshot/history', (route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: '[]' })
  );

  // Mock the SSE stream: return a well-formed event then close.
  // The first event seeds the portfolio so stat panels get real values.
  const sseEvent = [
    'data: ' + JSON.stringify({
      portfolio: {
        Timestamp: new Date().toISOString(),
        Cash: 100_000,
        Positions: {},
        RealizedPnL: 0,
        UnrealizedPnL: 0,
        TotalValue: 100_000,
        TotalFills: 0,
        TotalCosts: 0,
      },
      halted: false,
      fills: [],
    }),
    '',
    '',
  ].join('\n');

  await page.route('**/api/stream', (route) =>
    route.fulfill({
      status: 200,
      contentType: 'text/event-stream',
      headers: { 'Cache-Control': 'no-cache' },
      body: sseEvent,
    })
  );
});

test('smoke: page loads and renders key panels', async ({ page }) => {
  await page.goto('/');

  // Header brand is visible.
  await expect(page.getByRole('heading', { name: 'HUGINN' })).toBeVisible();

  // Status indicator shows connection state (either is fine — the test is
  // that the UI renders, not that the SSE is live in this stub environment).
  const statusText = page.locator('.status-indicator');
  await expect(statusText).toBeVisible();

  // Circuit breaker banner is rendered.
  const banner = page.locator('.banner');
  await expect(banner).toBeVisible();

  // Portfolio stats grid is rendered with at least one panel.
  // Use .first() because App.tsx has two .stats-grid containers:
  // the outer portfolio grid and the inner strategy-panel sub-grid.
  const statsGrid = page.locator('.stats-grid').first();
  await expect(statsGrid).toBeVisible();
  await expect(statsGrid.locator('.panel').first()).toBeVisible();

  // "Total Equity Value" label is in the DOM.
  await expect(page.getByText('Total Equity Value')).toBeVisible();

  // Strategy panel renders (config was seeded via stub).
  await expect(page.getByRole('heading', { name: 'Active Strategy' })).toBeVisible();

  // Equity curve panel renders.
  await expect(page.getByRole('heading', { name: 'Real-time Portfolio Equity Curve' })).toBeVisible();
});

test('smoke: strategy panel shows seeded config values', async ({ page }) => {
  await page.goto('/');

  // The strategy name from stubConfig should appear somewhere in the panel.
  // App renders it as a stat-val inside the Active Strategy panel.
  await expect(page.getByText('obi')).toBeVisible();
});
