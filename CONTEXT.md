# Mithril Codebase Context (Quick Orientation)

This file is a lightweight entry point to the repo and how to approach changes.
For deeper epoch-boundary logic or active issues, use the linked docs below.

## AI Guidelines (Short)

- Validate behavior against **Agave** and **Firedancer**; cite file:line.
- If unsure, say so explicitly and propose a concrete verification step.
- Always include slot/epoch context for boundary-sensitive values.

Review request template:

```
You are reviewing Mithril for Agave/Firedancer parity.
Goal: <exact target behavior>.
Context: snapshot slot S, boundary slot B, first reward slot F, epoch E.
Branch/commit: <hash>. Logs: <paste 5–15 lines>.
Code pointers: <file:line>, <file:line>.

Please:
1) List likely causes in priority order (with confidence).
2) For each cause, point to FD/Agave code paths and our code paths.
3) Propose the minimal fix + how to verify it.
4) If unsure, say so and give a concrete test/diagnostic.
```

## Where to Start

- Epoch boundary details: `EPOCH_TRANSITION_OVERVIEW.txt`
- Active issues / TODO: `EPOCH_TRANSITION_TODO.txt`
- Architecture notes (longer-term, non-AI friendly): `architecture/`

## Change Approach (Alignment First)

- When in doubt, validate behavior against **Agave** and **Firedancer** code.
- If unsure, say so explicitly and propose a concrete verification step.
- Always note which **slot/epoch** a value comes from (boundary slot vs
  first reward slot vs snapshot slot). Most mismatches trace back to this.
- Keep logs + RPC comparisons as sanity checks, but avoid using RPC as a
  permanent crutch unless upstream behavior demands it.
- Add new design notes to `architecture/` as features stabilize.

## Repo Map (High Level)

- `cmd/mithril/` - CLI entrypoints (`mithril run`, `mithril state`, etc.)
- `pkg/replay/` - replay loop, epoch boundary logic, schedule handling
- `pkg/rewards/` - staking rewards, partitions, points computation
- `pkg/leaderschedule/` - leader schedule generation + validation
- `pkg/accountsdb/` - AccountsDB, appendvec parsing, index lookups
- `pkg/snapshot/` - snapshot manifest decode, AccountsDB build
- `pkg/rpcclient/` - RPC fetchers (blocks, schedules, sysvars)
- `architecture/` - evolving design docs for humans

## Current Branch Context

- Repo root: `mithril-dev`
- Branch: `feature/local-rewards-partitions`
- Local HEAD (dev machine): `c3454f146f3a7d66cab0f04c9dcf7063d97302d7`
- Local uncommitted files: `CONTEXT.md`, `EPOCH_TRANSITION_OVERVIEW.txt`,
  `EPOCH_TRANSITION_TODO.txt`
- Production runs referenced in docs were on the Ubuntu node (logs in
  `/mnt/mithril-logs/...`).

## Working Baseline (This Branch)

- Epoch boundary resume detection uses the last completed slot and a minimal
  `SlotCtx` to avoid nil deref crashes.
- Leader schedule computation matches Agave/Firedancer (ChaCha RNG + vote-keyed).
- Snapshot schedule reuse at the boundary avoids rebuilding the current epoch
  with the wrong stake epoch.
- Stake cache is persisted for resume, with fallback AccountsDB scan if missing.

## For Epoch-Boundary Details

See `EPOCH_TRANSITION_OVERVIEW.txt` for the end-to-end flow, examples, and
previous failure modes. That doc is the canonical handoff for the boundary
logic and resume behavior.
