# Mithril Compatibility

## Policy

- Mithril uses independent semver, not tied to Agave patch versions.
- A release is compatible with a network only if the expected feature gates match.
- Testnet and mainnet support are tracked separately.

## Release Matrix

| Mithril | Date | Agave baseline | Firedancer baseline | Mainnet | Testnet | Notes |
|---------|------|----------------|---------------------|---------|---------|-------|
| v0.1.0-alpha.1 | 2026-01-15 | v3.0.14 | v0.808.30014 | supported | not supported | first public alpha |

## Feature Gates

Feature gate compatibility will be documented in future releases.

## Divergences

- Testnet is not supported for this release.
