# Mithril Compatibility

## Policy

- Mithril uses independent semver, not tied to Agave patch versions.
- A release is compatible with a network only if the expected feature gates match.
- Testnet and mainnet support are tracked separately.

## Release Matrix

| Mithril | Date | Agave baseline | Gate set | Mainnet | Testnet | Notes |
|---------|------|----------------|----------|---------|---------|-------|
| v0.1.0-alpha.1 | TBD | 3.0.x | mainnet-alpha-TBD | supported | not supported | first public alpha |

## Gate Set: mainnet-alpha-TBD

Expected to match Solana mainnet feature gate state at release time.

| Gate name | Feature pubkey | Expected state | Required | Notes |
|-----------|----------------|----------------|----------|-------|
| TBD | TBD | TBD | TBD | Fill in before release |

## Network State Snapshot

- mainnet: slot TBD, rpc TBD

## Divergences

- Testnet is not supported for this release.
