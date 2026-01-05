# Mithril Architecture Documentation

This directory contains detailed documentation about Solana protocol internals as implemented in Mithril. The focus is on protocol-level concepts that are shared across validator implementations (Agave, Firedancer, Mithril) rather than implementation-specific details.

## Documents

### Core Consensus & Rewards
- [Partitioned Epoch Rewards](partitioned-epoch-rewards.md) - How staking rewards are calculated and distributed across multiple blocks
- [Stake Accounts](stake-accounts.md) - Stake delegation lifecycle, warmup/cooldown, and the stake cache

### Leader Selection
- [Leader Schedule](leader-schedule.md) - How the leader schedule is computed from stake weights (TODO)

### Accounts & State
- [AccountsDB](accountsdb.md) - Account storage, snapshots, and state management (TODO)
- [Bank Hash](bankhash.md) - How the bank hash is computed for consensus (TODO)

## Design Philosophy

These documents aim to:
1. **Explain the "why"** - Not just what the code does, but why Solana designed it this way
2. **Be implementation-agnostic** - Focus on protocol behavior that all clients must match
3. **Include edge cases** - The tricky parts that cause bugs when implementing
4. **Reference authoritative sources** - Link to Agave code, SIMDs, and other specifications

## Contributing

When adding documentation:
- Start with the protocol behavior, then implementation details
- Include worked examples where helpful
- Document known edge cases and their correct handling
- Cross-reference related systems
