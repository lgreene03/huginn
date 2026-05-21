# Contributing to Huginn

Thank you for considering a contribution. This document covers the mechanics; the design philosophy lives in `docs/ROADMAP.md` and `internal/strategy/strategy.go`.

---

## Ground rules

- **One concern per PR.** Bug fixes, new features, and refactors should not mix.
- **Tests first.** Every new exported function needs a test. Every bug fix needs a regression test.
- **No `--no-verify`.** Pre-commit hooks are not optional.
- **Roadmap phases are ordered.** Do not implement Phase N+1 items while Phase N is incomplete.

---

## Development setup

```bash
git clone https://github.com/lgreene03/huginn.git
cd huginn
go mod download
go test -race ./...
```

Requires Go 1.23+. A running PostgreSQL instance is needed for Postgres-mode tests (see `internal/journal/`); set `DATABASE_URL` in your environment or use the bundled `docker-compose.yml`:

```bash
docker-compose up -d postgres
export DATABASE_URL=postgres://postgres:postgres@localhost:5432/huginn?sslmode=disable
export DATABASE_ENABLED=true
go test -race ./...
```

---

## Code style

- `gofmt` and `goimports` are required. Run `gofmt -w .` before committing.
- `go vet ./...` must pass.
- `golangci-lint run` (see `.golangci.yml`) must pass.
- Default to **no comments**. Only add a comment when the *why* is non-obvious.

---

## Adding a strategy

1. Create `internal/strategy/<name>.go` implementing `strategy.Strategy`.
2. Implement `Stateful` (`MarshalState`/`RestoreState`) if the strategy holds mutable accumulator state that must survive a restart.
3. Wire the strategy name in `cmd/huginn/main.go`'s `switch cfg.Strategy.Name` block.
4. Add calibration support to `cmd/calibrate/main.go`.
5. Add at least three tests: happy-path signal, no-signal (below threshold), and state round-trip if `Stateful`.

---

## Adding a database migration

Append a new entry to `pgMigrations` in `internal/journal/pg_migrations.go`. Never edit or remove existing entries. Migrations run automatically on boot.

---

## Pull request checklist

- [ ] `go build ./...` passes
- [ ] `go test -race ./...` passes
- [ ] `go vet ./...` passes
- [ ] `golangci-lint run` passes
- [ ] New public functions have tests
- [ ] ROADMAP updated if a phase deliverable is complete
