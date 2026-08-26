# Rooted event feed

Mithril can write a local JSON Lines feed of finalized transactions, account
updates, and slot roots. The feed is intended for custom indexes and local
agent tools that need a replayable source instead of polling account RPCs.

The feature is off by default.

## Enable capture

Add this to `config.toml` before bootstrapping the AccountsDB:

```toml
[storage]
rooted_events = true
```

On the Alpenglow community cluster, Mithril publishes the prefix selected by
Alpenglow finality. On `mainnet-beta`, `testnet`, and `devnet`, capture requires
finalized RPC input and the required trailing verifier:

```toml
[block]
source = "rpc"

[verifier]
enabled = true
required = true
```

The storage profile belongs to the AccountsDB lineage. A Classic AccountsDB
that has already advanced with per-slot writes cannot be converted in place.
Build a fresh AccountsDB in a separate directory when enabling the feed on a
Classic cluster. A rooted-durable lineage must keep the option enabled.

## Read events

Start at the current tail and wait for new roots:

```bash
./mithril events --latest --follow --framed
```

Read an existing retained suffix:

```bash
./mithril events --framed
```

Resume after a saved cursor:

```bash
./mithril events --after 312450:17 --follow --framed
```

`--accounts` can select an AccountsDB root without loading `config.toml`:

```bash
./mithril events --accounts /mnt/mithril-accounts --latest --follow --framed
```

Useful filters are:

- `--owner PROGRAM_ID` for final account values owned by one program
- `--account ADDRESS` for one account
- `--mention ADDRESS` for transactions whose resolved account list contains an address

Slot-root records are kept when filtering because they close the selected
slot and carry its lineage.

## Framed output

`--framed` adds three metadata record types to the event stream:

- `mithril.rooted_source` identifies the cluster, genesis hash, and AccountsDB root run
- `mithril.rooted_start` records the exclusive starting cursor
- `mithril.rooted_batch` identifies the selecting fold manifest and immutable sidecar SHA-256

The source and start records appear once per process. A batch record appears
before that batch's events. Starting at the exact tail prints nothing until a
new event is available.

Event records use schema version 3:

- `transaction_executed` carries the full signed transaction wire, message hash,
  resolved account keys, execution result, compute units, logs, inner
  instructions, and return data when present
- `account_updated` carries the final account value for that rooted slot
- `slot_rooted` closes the slot with its blockhash, parent blockhash, bankhash,
  parent slot, finality source, and Alpenglow block identities when applicable

`finality_source` is one of `alpenglow_certificate`, `alpenglow_delegated`, or
`rpc_finalized`. It states which node path selected the root. It is not a copy
of certificate bytes, and it may change between rooted slots.

## Consumer rules

Treat the cursor as `(slot, ordinal)` and save it only after the corresponding
record is durable in your index. Pass that cursor back through `--after` after
a restart.

For a framed stream, pin all of these values:

- source cluster
- source genesis hash
- AccountsDB root run ID
- batch manifest sequence and SHA-256

The reader verifies fold-manifest CRCs and sidecar size and SHA-256 before it
decodes records. It also checks parent slot, parent blockhash, and Alpenglow
parent block ID across batch boundaries.

Retention can move past an old cursor. The command then exits with a cursor-gap
error and reports the earliest retained cursor. Rebuild that index from the
available suffix or restore older retained artifacts; do not silently skip the
gap.

## RPC identity check

When capture is enabled, `getRootedFeedStatus` returns the AccountsDB root run
ID used by framed output:

```bash
curl http://127.0.0.1:8899 -H 'content-type: application/json' -d \
  '{"jsonrpc":"2.0","id":1,"method":"getRootedFeedStatus"}'
```

This method identifies configuration and lineage. Use `getHealth` and
`getVerificationStatus` for replay and verification health.

## Operational notes

- The command is read-only and writes JSON Lines to stdout.
- It opens the configured AccountsDB directory but not the mutable account index.
- One long-running `--follow` process reuses verified manifest and sidecar state;
  an unchanged tail does not reread the full sidecar every second.
- Event files are immutable and become authoritative only when a committed fold
  manifest selects them.
- The feed contains finalized state, not speculative processed-bank updates.
