# 0001. Record architecture decisions

- **Status:** Accepted
- **Date:** 2026-06-20
- **Deciders:** Project maintainer
- **Related:** [CLAUDE.md](../../CLAUDE.md), [architecture.md](../architecture.md), [ROADMAP.md](../ROADMAP.md)

## Context

Huginn has a set of living documents that describe how the system *is* — [`architecture.md`](../architecture.md), [`strategies.md`](../strategies.md), [`risk-model.md`](../risk-model.md), [`STRATEGY_STATE_DESIGN.md`](../STRATEGY_STATE_DESIGN.md). These are evergreen: they are kept current as the engine evolves and describe the present state of the world.

They do not, by themselves, record *why* the world is that way. When a future contributor asks "why is the hot path single-goroutine?", or "why did we build the gRPC service without a `.proto` file and `protoc`?", the doc says *what* we do, not *what we considered and why we rejected the alternatives*.

That history is valuable. Without it, future contributors re-litigate decisions that were already made, or reverse them without understanding what they cost. The sibling [Muninn](../../../muninn/docs/adr/) repository already keeps an ADR set; Huginn had none, so the reasoning behind its more unusual choices (dynamic-proto gRPC, dual-mode executor, the deliberately confidence-aware walk-forward output) lived only in code comments and commit messages.

## Decision

Huginn records significant architectural decisions as **Architecture Decision Records (ADRs)** in [`docs/adr/`](.). Each ADR is a short Markdown file following the template in [`0000-template.md`](0000-template.md), reusing the conventions established in Muninn.

ADRs are:

- **Numbered sequentially.** Filename: `NNNN-short-title.md`.
- **Immutable in spirit.** Once accepted, an ADR is not edited except for typos, status changes, or links to superseding ADRs.
- **Superseded, not deleted.** A reversed decision results in a new ADR that supersedes the old one. Both remain in the repository.
- **Concise.** One to two pages. If longer, the decision is probably two decisions.

An ADR is warranted when a change adds/removes/replaces a load-bearing dependency, introduces or removes a service boundary, changes a property documented as load-bearing in the architecture doc, or reverses a previous ADR. Routine implementation choices, bug fixes, and doc updates need no ADR.

## Consequences

**Easier.** Future contributors (and AI agents onboarding to the repo) can reconstruct the reasoning behind the engine as it stands. Reversing a decision becomes a deliberate act with a paper trail.

**Harder.** Every significant decision now requires a written record — a small, intended friction on architectural change.

**Relationship to the living docs.** The living docs describe the *current* system; ADRs describe *decisions and their reasoning at a moment in time*. When an ADR is accepted, the relevant doc is updated in the same change. The initial batch (ADR-0002 through ADR-0007) is retrospective: it documents decisions already embodied in the code, grounded in the files that implement them.

## Alternatives Considered

- **Git commit messages only.** Rejected. Commit messages are not discoverable; nobody reads `git log` to learn an architecture.
- **Code comments only.** Rejected. Huginn's comments are unusually thorough (see `executor.go`, `walkforward/main.go`), but they explain *local* mechanism, not cross-cutting decisions or rejected alternatives.
- **No ADRs.** Rejected. Muninn already has them; an asymmetric Norse stack where only one service records its decisions is the worse outcome.

## References

- Michael Nygard, ["Documenting Architecture Decisions"](https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions) (2011).
- [adr.github.io](https://adr.github.io/) — community conventions.
- [Muninn ADR set](../../../muninn/docs/adr/) — the sibling repository's records, whose template this reuses.
