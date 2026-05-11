# Confirmed-Block Fork-Choice Heuristic

Mithril uses a lightweight confirmed-block fork-choice heuristic when replaying recent blocks from Lightbringer.

The goal is to decide which locally observed blocks should be executed before Mithril commits to executing them. This allows Mithril be independent of RPC providers for recent blocks (except for catch-up purposes): Lightbringer supplies the block data from Turbine and Repair, and Mithril parses vote transactions in received blocks to derive confirmed-block information, in addition to observing parent links to decide upon the correct execution path.

These heuristics allow Mithril to select a PoH-consistent path of blocks and skipped slots. Mithril then executes selected blocks and verifies the resulting bankhash for consistency against the one expected based on vote tallies.

## What Problem It Solves

When Mithril is close to the tip of the chain, Lightbringer may observe:

- blocks arriving from Turbine,
- blocks recovered through Repair,
- skipped slots,
- temporary gaps,
- and blocks ahead of the last slot Mithril has executed.

Before executing forward, Mithril needs to know which observed blocks are on the confirmed path.

Instead of asking an RPC provider for every recent block body, Mithril can use Lightbringer’s local block stream and ask:

> Given my current execution anchor, which observed blocks connect to a confirmed leaf?

The answer is a sequence of slot decisions:

```text
slot n       -> use block
slot n + 1   -> skipped
slot n + 2   -> skipped
slot n + 3   -> use block
```

Mithril then executes the selected blocks and skips the selected empty slots.

## Terminology

In this context, **blockhash** means the Solana PoH blockhash: the final PoH/entry hash for a slot.

It does **not** mean the bankhash produced by executing the block.

Mithril tracks both concepts:

- **PoH blockhash**: used to connect slots and reason about skipped-slot paths.
- **Bankhash**: produced after executing the block and used to verify execution correctness.

The fork-choice heuristic works with observed PoH parent relationships and vote-confirmed bankhash winners. Final execution correctness is still checked by Mithril’s bankhash verification path.

## How The Heuristic Works

At a high level, Mithril:

1. Starts from the current execution anchor.
2. Observes blocks from Lightbringer before executing them.
3. Extracts vote information from those observed blocks.
4. Tracks which bankhash has reached supermajority for observed slots.
5. Finds the highest confirmed leaf reachable within the configured depth.
6. Walks backward from that confirmed leaf to the execution anchor using observed parent links.
7. Converts the parent chain into a list of slot decisions.
8. Executes blocks marked `UseBlock = true`.
9. Treats missing intermediate slots as skipped.

The important detail is that the implementation does **not** brute-force every possible PoH branch.

Instead, it uses the parent metadata already available from observed blocks:

- Lightbringer blocks provide a known parent slot.
- RPC/all-source blocks can be linked by parent blockhash once Mithril has a valid PoH anchor.
- Missing slots between a child and its parent are treated as skipped.

So the resolver is better described as a **backward parent-link path resolver**, not a forward exhaustive PoH search.

## Execution Anchor

The search starts from Mithril’s current execution anchor.

The anchor can come from:

- the last successfully executed slot during normal replay,
- persisted resume state after a restart,
- or the snapshot’s latest PoH blockhash on a fresh start.

The snapshot bankhash is not used as a PoH anchor. Bankhash and PoH blockhash are separate values with different roles.

The relevant anchor logic lives in:

- `pkg/replay/block.go`
  - `consensusExecutionAnchor(...)`
  - `observeConsensusAnchor(...)`

## Slot Decisions

The resolver returns a list of slot decisions:

```text
UseBlock = true   -> execute the observed block at this slot
UseBlock = false  -> treat this slot as empty/skipped
```

Example:

```text
anchor: slot 100

observed:
  slot 101 parent = 100
  slot 104 parent = 101

resolved path:
  slot 101 -> use block
  slot 102 -> skipped
  slot 103 -> skipped
  slot 104 -> use block
```

This means the confirmed path from slot 100 to slot 104 uses the observed blocks at 101 and 104, while treating 102 and 103 as skipped.

A skipped decision does not mean no validator ever saw any data for that slot. It means the selected confirmed path does not include a block at that slot.

## Relationship To Votes

Mithril observes vote transactions inside candidate blocks and uses stake-weighted vote information to identify confirmed leaves.

The fork-choice service tracks:

- observed block metadata,
- vote-derived stake totals,
- supermajority bankhash winners,
- parent relationships,
- equivocations,
- and pending parent-blockhash links.

Once a slot has a supermajority winner, the consensus coordinator can try to resolve a path from the current anchor to that confirmed slot.

Relevant code:

- `pkg/forkchoice/forkchoice.go`
  - `ObserveBlock(...)`
  - `FindConfirmedLeaf(...)`
  - `ResolvePathToLeaf(...)`
  - `IsBankhashCorrect(...)`

- `pkg/forkchoice/vote_parser.go`

- `pkg/forkchoice/vote_stake_accumulator.go`

## Path Resolution

The path resolver walks backward from a confirmed leaf slot toward the anchor.

For each observed block, it checks the block’s parent slot. Any gap between the child slot and parent slot becomes skipped slots in the resolved path.

Example:

```text
anchor = 200

confirmed leaf:
  slot 205 parent = 202

observed parent:
  slot 202 parent = 200

resolved path:
  201 -> skipped
  202 -> use block
  203 -> skipped
  204 -> skipped
  205 -> use block
```

Relevant code:

- `pkg/forkchoice/skip_path.go`
  - `ResolvePohPath(...)`

- `pkg/forkchoice/consensus_coordinator.go`
  - `ResolveFromAnchor(...)`

## Replay Integration

During replay, Mithril buffers candidate blocks instead of immediately executing them when confirmed-block enforcement is active.

Then it:

1. observes candidate block metadata,
2. feeds votes into the fork-choice service,
3. asks the consensus coordinator for a resolved path,
4. executes only the blocks selected by that path,
5. records skipped slots as replay progress,
6. and verifies bankhash after execution.

Relevant code:

- `pkg/replay/block.go`
  - `observeBlockForConsensus(...)`
  - `syncConsensusBufferedExecutionMode(...)`
  - `readyConsensusPath`
  - `pendingConsensusPath`

## Configuration

The heuristic is configured under Mithril’s `[consensus]` section.

Example:

```toml
[consensus]
skip_path_max_depth = 64
unresolved_policy = "halt"
enforce_on_source = "lightbringer"
```

Key options:

- `skip_path_max_depth`
  - Maximum number of slots the resolver will search from the current anchor.

- `unresolved_policy`
  - `"halt"`: stop gracefully if the path cannot be resolved.
  - `"warn"`: log and continue. Mainly useful for debugging.

- `enforce_on_source`
  - `"lightbringer"`: enforce confirmed-block path selection for Lightbringer blocks.
  - `"all"`: enforce across all block sources that can be linked by parent slot or parent blockhash.

Relevant code:

- `pkg/config/config.go`
- `cmd/mithril/node/node.go`
- `pkg/replay/block.go`

## What This Heuristic Provides

This heuristic provides:

- a confirmed execution path from the current anchor,
- local use of Lightbringer block data,
- reduced dependence on RPC block bodies for recent slots,
- explicit skipped-slot decisions,
- and a bounded way to avoid executing blocks before Mithril knows they connect to a confirmed path.

It is especially useful when Mithril is replaying near the tip and Lightbringer is receiving fresh Turbine/Repair data locally.

## What It Does Not Prove

This heuristic does not prove that a block executed correctly.

It does not replace:

- transaction execution,
- account loading,
- bankhash calculation,
- reward calculation,
- rent handling,
- or final bankhash verification.

It also should not be described as a full consensus proof. It is a practical, bounded fork-choice heuristic used to select a local execution path before Mithril performs full execution verification.

## Failure Modes

Path resolution can fail when:

- the target confirmed block has not arrived yet,
- intermediate shreds are missing,
- repair has not completed,
- the confirmed target is outside the configured search depth,
- parent metadata is incomplete,
- observed blocks conflict,
- vote data has not landed yet,
- or the local execution anchor is wrong.

When the path is incomplete, Mithril can wait for more Lightbringer data or repair progress.

When the path cannot be resolved within the configured bounds, Mithril follows the configured policy, such as halting gracefully or warning for debugging.

## Summary

Lightbringer gives Mithril recent block data from Turbine and Repair.

Mithril’s confirmed-block fork-choice heuristic determines which observed blocks connect to a confirmed path.

Mithril then executes that path and verifies the resulting bankhash.

The heuristic is implemented in Mithril today, primarily in:

- `pkg/forkchoice/forkchoice.go`
- `pkg/forkchoice/skip_path.go`
- `pkg/forkchoice/consensus_coordinator.go`
- `pkg/replay/block.go`
