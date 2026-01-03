<!--
Purpose: concise, actionable guidance for AI coding agents (Copilot-like)
Focused on this repository's build, run, architecture, and patterns.
-->
# Copilot / AI Agent Hints for the Mithril repo

This file captures repository-specific knowledge an AI coding agent needs to be productive.

**Quick Start Commands:**
- Build: `make build` (runs `go build -ldflags ... -o mithril ./cmd/mithril`).
- Run: `./mithril run` (uses `config.toml` in CWD). Generate starter config: `./mithril config init`.
- Setup system (optional, interactive): `sudo ./scripts/disk-setup.sh --setup` and `sudo ./scripts/performance-tune.sh`.
- Rebuild after changes: `git pull && make build`.

**Big-Picture Architecture (discoverable in `cmd/`, `pkg/`):**
- `cmd/mithril` is the CLI entrypoint. `pkg/` contains core subsystems: `accounts` (AccountsDB), `blockstore` (block persistence), `snapshot`/`snapshot-finder` logic, RPC server code.
- Main data flows: snapshot download -> AccountsDB bootstrap -> block fetch (currently via RPC `getBlock`) -> replay/VM execution -> optional RPC server serving account state.
- Important design notes: AccountsDB is heavy on random I/O (put it on the fastest NVMe). Block persistence is currently disabled/stream-only in config comments.

**Key Files & Directories to Reference:**
- `README.md` — operational guidance and required Go/C toolchain notes.
- `Makefile` — canonical build targets and LDFLAGS that embed version info into `pkg/version`.
- `config.example.toml` — canonical runtime configuration and tuning knobs (bootstrap modes, storage paths, RPC endpoints).
- `scripts/` — system setup, disk benchmarking and tuning; many scripts are interactive and require `sudo`.
- `go.mod` — module/go/toolchain version and important `replace` directives (useful when fixing dependency issues).

**Developer Workflows & Constraints:**
- Go toolchain: follow `go.mod` for the required Go version; README suggests Go >= 1.25 for GC improvements, but `go.mod` currently declares `go 1.24` and `toolchain` lines — prefer `go.mod` as authoritative for builds.
- CGO/system deps: repository uses CGO-using deps (rocksdb/grocksdb). Building on Linux requires a C toolchain (`build-essential`) and native libs; README explicitly requires a C compiler.
- Tests: unit and conformance tests exist (see `conformance/` and `pkg/...`). Typical command: `go test ./...` or focus `go test ./conformance/...`.

**Patterns & Conventions (project-specific):**
- Version embedding: builds use `-ldflags` to set `pkg/version.{Version,GitCommit,BuildDate}` via `Makefile`.
- Config-first runtime: prefer `config.example.toml` and `mithril config init` when adding runtime flags; many runtime behaviors are gated in config (e.g., `bootstrap.mode`, `snapshot.max_full_snapshots`).
- Safety: several scripts will erase disks — always surface `scripts/*` usage to humans and avoid non-interactive destructive runs.

**Integration Points & External Dependencies:**
- Runtime uses external RPC endpoints (configured under `[network].rpc`). Catchup currently relies on RPC `getBlock`; changes here affect network IO patterns and rate limiting logic in `block` config.
- Replacements in `go.mod` indicate forks/patches (check `replace` lines when debugging version or API mismatches).

**When Making Changes, Prefer These Files For Context:**
- `cmd/mithril` for CLI flags and wiring.
- `pkg/version` for version embedding and LDFLAG-sensitive behavior.
- `config.example.toml` to document and add new runtime knobs.
- `scripts/*` for ops implications (disk layout, tuning, benchmarks).

**Examples:**
- Add a configuration flag: update `config.example.toml`, add parsing in the appropriate `pkg/*` config loader, and wire CLI defaults in `cmd/mithril`.
- Add telemetry: hook into existing `reporting` or `prometheus` packages (see `pkg/*` for client usage patterns).

If any section is unclear or you want deeper examples (e.g., how AccountsDB is accessed in code, or where block fetching is implemented), tell me which subsystem to expand and I will iterate.
