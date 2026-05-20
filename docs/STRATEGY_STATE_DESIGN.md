# Strategy-State Journal — Design

_Companion to Phase 1 of `docs/ROADMAP.md`. Goal: strategies survive a restart bit-for-bit, like the portfolio already does._

## 1. State to persist, per strategy

**[E]** essential for correctness on restart, **[N]** nice-to-have.

**OBIThreshold** (`internal/strategy/obi_threshold.go`)
- `Threshold`, `OrderSize`, `maxPosition` — **[N]** config-sourced; persist for drift detection only.
- `netPosition` — **[E]** drives throttle gating; without it, restart bypasses position limit.

**VPINBreakout** (same file, line 92+)
- `Threshold`, `OrderSize`, `cooldown` — **[N]**.
- `lastTrade` — **[E]** without it, cooldown is bypassed on restart.

**VWAPDeviation**
- `ThresholdPct`, `OrderSize`, `maxPosition` — **[N]**.
- `netPosition` — **[E]**.

**EMACrossover**
- `FastPeriod`, `SlowPeriod`, `OrderSize`, `maxPosition` — **[N]**.
- `fastEMA`, `slowEMA` — **[E]** decayed price history; not cheaply reconstructable.
- `prevFastEMA`, `prevSlowEMA` — **[E]** required to detect the very next crossover.
- `count` — **[E]** gates warmup branch and first-sample seeding.
- `netPosition` — **[E]**.

Config fields are **[N]** even though `UpdateConfig` mutates them at runtime — config-on-disk wins on restart, a delta logs a WARN.

## 2. When to persist

**Hybrid: write on every fill, plus a coalescing 5-second ticker for strategies whose state mutates without fills (EMA).**

The executor calls `persistStrategyState()` immediately after `journalWriter.Append(fill)` succeeds, and a separate goroutine fires the same call every 5 s if a `dirty` bit is set. Position-bearing state changes only land on fills; EMA's continuous mutation gets bounded 5 s RPO, which is acceptable for paper-trading.

## 3. Where to persist

**Same backend, separate stream.** Fate-sharing with the fill journal: if the journal is down, refuse to trade regardless of which record type failed.

**JSONL** — tagged-record stream. Today every line is a bare `Fill`. Going forward:
```json
{"type":"fill", ...}
{"type":"strategy_state","strategy_key":"obi","schema_version":1,"blob":<inline json>,"updated_at":"..."}
```
Legacy untagged lines are treated as fills (see §7).

**Postgres** DDL:
```sql
CREATE TABLE IF NOT EXISTS strategy_state (
    strategy_key    VARCHAR(64) PRIMARY KEY,
    schema_version  INT NOT NULL,
    state_blob      JSONB NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_strategy_state_updated ON strategy_state(updated_at);
```
Upsert on `strategy_key`. Single-row-per-strategy — we don't need history; the portfolio table gives reconstructable PnL. `schema_version` is **mandatory from day one**.

## 4. Restore order on boot

1. Load config (decides which strategy is active).
2. Recover portfolio from fills (existing path).
3. Instantiate strategy from config (defaults).
4. Load `strategy_state` row for the configured strategy's stable key.
   - Present + version matches binary → `RestoreState(blob)`.
   - Absent → log INFO "no prior state, starting fresh".
   - `strategy_key` differs from configured → log WARN, do **not** restore (cross-loading EMA state into OBI is nonsense).
   - Version older → run the migration registry (§8).
   - Version newer than binary supports → refuse to boot.

Portfolio first, then strategy. If strategy's `netPosition` disagrees with portfolio's quantity, log WARN with both — do not auto-reconcile.

## 5. Failure modes

| Scenario | Behavior |
|---|---|
| `MarshalState` fails | log ERROR, `huginn_strategy_state_marshal_errors_total++`, continue trading |
| Write fails (disk full, PG down) | log ERROR, `huginn_strategy_state_persist_errors_total++`, continue |
| `RestoreState` fails at boot | **refuse to boot**; bad state is worse than no state. Escape hatch: `--ignore-strategy-state` flag |
| Persist succeeds, crash before next event | happy path — re-load that exact state on next boot |
| Persist of fill succeeds, persist of state fails | tolerable; §4 reconciliation WARN catches it, self-corrects on next persist |

## 6. Concurrency

Today: `EMACrossover` self-mutexes, the other three don't. Executor calls `OnFeature` single-threaded (Kafka consumer), so no race today — EMA's mutex is defensive against the HTTP `UpdateConfig` path (a latent bug; see §10).

Adding `MarshalState` introduces a second reader, possibly from a different goroutine (the 5 s ticker). **Recommendation: methods are self-synchronizing.** Each strategy gets a `sync.Mutex`; `OnFeature`, `MarshalState`, `RestoreState` all acquire it. Cost: one uncontended lock per event — negligible. Externalizing the lock would leak strategy concerns into the executor.

## 7. Migration story

Existing JSONL files contain bare `Fill` records. The reader, on encountering a line that doesn't parse as `{"type":...}`, falls back to parsing as `Fill` (preserves today's behavior). PG deployments simply have no `strategy_state` table until `initSchema` creates it on first boot of the new binary.

Fresh boot against an old journal starts strategies with default state and logs one WARN: _"no prior strategy_state found; strategy will start from zero — expected on first upgrade"_. Operators see it once.

## 8. Schema-evolution discipline

Blob format: `{"version":N, "fields":{...}}`. Rules:
- **Add a field** → bump version; `migrateV(N)_to_V(N+1)` supplies a default. Old binary refuses new state; new binary migrates old state up.
- **Remove a field** → leave in schema, ignore on read. No bump.
- **Rename/retype** → bump, write a migrator.

Each strategy owns a registry `map[int]func(json.RawMessage) (json.RawMessage, error)` keyed by source version. Restore loops migrations until current.

## 9. Test strategy

One file per strategy (`obi_threshold_state_test.go` etc.):

1. **Round-trip equivalence** (the contract). Feed strategy A `N` events, marshal. Construct B from same config, `RestoreState`. Feed B `M` more events. Construct C from scratch, feed all `N+M` events. Assert: A's tail orders ≡ B's orders, and final marshaled state ≡ C's.
2. **Default-on-missing**: `RestoreState(nil)` returns a typed error, no mutation.
3. **Version-mismatch**: `RestoreState` with `version=999` returns a typed error.
4. **Migration**: hand-craft a v1 blob, restore with vN binary, assert fields populated.
5. **Concurrency**: `go test -race` with concurrent `OnFeature` + `MarshalState`; no race.
6. **Integration**: end-to-end boot test — run executor for K events with `t.TempDir()` JSONL, kill, re-boot, assert next event behaves as if uninterrupted.

## 10. Open questions

1. Should `UpdateConfig` invalidate persisted state? If `Threshold` changes mid-flight, `netPosition` is still valid but the throttle bound (`maxPosition`) may now disagree — re-validate?
2. Where does the 5 s ticker live — executor, or a new `StateManager`? **Recommendation:** new type, executor owns one.
3. The latent `UpdateConfig` race — fix here, or out of scope? **Recommendation:** out of scope, file separately, but the per-strategy mutex we add for `MarshalState` should also cover config mutations.
4. PG `strategy_state_history` audit table, or latest-wins enough? **Recommendation:** skip until requested.
5. For VPIN, `lastTrade` is event-time. Restoring after long downtime voids cooldown. **Intended** — event-time semantics are right — but document it.
6. `Name()` includes parameters (`OBIThreshold(0.70)`). If operator changes threshold, the name changes and old state row is orphaned. **Use a stable identifier** (the config key, e.g. `"obi"`).

## Minimal code sketch

```go
// internal/strategy/stateful.go
type Stateful interface {
    MarshalState() ([]byte, error)
    RestoreState(data []byte) error
}

type stateEnvelope struct {
    Version int             `json:"version"`
    Fields  json.RawMessage `json:"fields"`
}

// internal/strategy/obi_threshold.go (additions)
type obiStateV1 struct {
    NetPosition float64 `json:"net_position"`
}
func (s *OBIThreshold) MarshalState() ([]byte, error) {
    s.mu.Lock(); defer s.mu.Unlock()
    f, _ := json.Marshal(obiStateV1{NetPosition: s.netPosition})
    return json.Marshal(stateEnvelope{Version: 1, Fields: f})
}
func (s *OBIThreshold) RestoreState(data []byte) error {
    var env stateEnvelope
    if err := json.Unmarshal(data, &env); err != nil { return err }
    if env.Version != 1 { return fmt.Errorf("unsupported version %d", env.Version) }
    var f obiStateV1
    if err := json.Unmarshal(env.Fields, &f); err != nil { return err }
    s.mu.Lock(); defer s.mu.Unlock()
    s.netPosition = f.NetPosition
    return nil
}

// internal/executor/executor.go (additions)
func (e *Executor) persistStrategyState() {
    sf, ok := e.strategy.(strategy.Stateful)
    if !ok { return }
    blob, err := sf.MarshalState()
    if err != nil { /* counter++ */ return }
    if err := e.journalWriter.AppendStrategyState(e.strategyKey, blob); err != nil { /* counter++ */ }
}
```

## Rejected alternatives

- **Protobuf for the blob.** JSON is already the journal's lingua franca; introducing protoc, generated code, and a new dependency for a few floats per strategy is unjustified. JSONB in Postgres is queryable in psql — opaque proto bytes are not.
- **Full event-sourcing replay from fills.** Tempting because the fill log exists, but EMA state is a function of every *price event*, not just fills, so fills are insufficient. Replay cost grows unboundedly with uptime. Snapshot semantics decouple boot time from history length.
- **Make `Stateful` mandatory on `Strategy`.** Most future strategies will be stateless or trivially so. Forcing a no-op on every implementer is noise and invites buggy stubs. Optional interface + type assertion is idiomatic Go (cf. `io.Closer`, `http.Flusher`).
