# Mithril

Mithril is a Solana full node client written in Golang with the goal of serving as a "verifying full node" with lower hardware requirements than that of Solana validators and RPC nodes. This project is being developed upon the foundations of [Radiance](https://github.com/firedancer-io/radiance), which was built by Richard Patel (@ripatel) with contributions from @leoluk.

This project is under active development. We are completing an audit with Runtime Verification and you can expect a more polished and feature-rich Alpha release in early Q1 2026. Until then, all code is likely to be incomplete, buggy, and/or improperly tested at any particular point in time. Please check the dev branch for the latest version of the codebase.

---

## Running Mithril (verify-live mode)

The `verify-live` command allows Mithril to bootstrap from a Solana snapshot and continuously verify new blocks as they are produced on mainnet-beta.

### Hardware Requirements

**Operating System**
- Ubuntu 24.04 LTS (recommended)

**CPU**
- Higher core speed Ryzen series recommended, at least 3.5 GHz base clock
- The 6-core AMD Ryzen 5 7640HS performs exceptionally well in our testing
- We haven't extensively tested a wide range of hardware yet - join the `#mithril-hardware` channel on the [Overclock Validator Discord](https://discord.gg/overclock) to discuss hardware configurations

**Storage**
- Minimum 1 TB PCIe 4.0 NVMe SSD (more is better)
- Two NVMe drives preferable for optimal performance:
  - **Fast NVMe**: Mithril's AccountsDB (requires high IOPS)
  - **Secondary NVMe**: Block storage and snapshots (can be slower)
- Samsung 990 Pro has been exceptional in our testing
- **Filesystem**: We are still testing optimal filesystem configurations (xfs vs ext4) - more guidance coming soon

**Network**
- Mithril works on home internet connections
- Slower internet speeds will result in longer snapshot download times
- The integrated snapshot finder automatically selects the fastest available snapshot source

### Getting Started

**Step 1: Clone the repository**

```bash
git clone https://github.com/Overclock-Validator/mithril.git
cd mithril
```

**Step 2 (Optional): Run setup scripts**

We provide helper scripts for setting up your system. See the [scripts documentation](scripts/README.md) for a detailed walkthrough:
- **Server Setup** - Fresh Ubuntu install (rescue mode) or security hardening (existing Ubuntu)
- **Disk Setup** - Benchmark NVMe drives, format with optimal settings, reset Mithril data
- **Performance Tuning** - Kernel settings, I/O scheduler, CPU optimization

**Step 3: Install Go 1.25 or later**

Go 1.25 introduced the "green tea" garbage collector improvements which provide better performance for memory-intensive applications like Mithril.

```bash
# Determine your architecture
ARCH=$(uname -m)
if [ "$ARCH" = "x86_64" ]; then
    GOARCH="amd64"
elif [ "$ARCH" = "aarch64" ]; then
    GOARCH="arm64"
else
    echo "Unsupported architecture: $ARCH"
    exit 1
fi

# Download and install Go 1.25
wget https://go.dev/dl/go1.25.0.linux-${GOARCH}.tar.gz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.25.0.linux-${GOARCH}.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
source ~/.bashrc
go version
```

**Step 4: Build Mithril**

```bash
go build -o mithril ./cmd/mithril
```

### Configuration

Create your configuration file by copying the example:

```bash
cp mithril.example.toml mithril.toml
```

Then edit `mithril.toml` to customize for your setup:

```toml
name = "mithril"

[ledger]
    # AccountsDB path - use your fastest NVMe
    accounts_path = "/mnt/mithril-accounts"

[rpc]
    # RPC endpoints for fetching blocks and reference slot
    endpoints = ["https://api.mainnet-beta.solana.com"]

[snapshot]
    # Optional: save snapshots to disk while streaming
    # save_to_disk = true
    # download_path = "/mnt/mithril-ledger/snapshots"

    # Verbose output shows detailed node discovery statistics
    # verbose = true
```

See `mithril.example.toml` for all available configuration options.

### Running verify-live

```bash
./mithril verify-live --config mithril.toml
```

**What happens:**
1. Mithril queries the Solana cluster to find available snapshot sources
2. A two-stage speed test identifies the fastest nodes
3. The full snapshot is streamed directly into memory and processed
4. An incremental snapshot is fetched to bring the state closer to the tip
5. Mithril begins fetching and verifying new blocks via RPC `getBlock` calls

### Directory Structure

Following Agave conventions, Mithril uses separate mount points:

```
/mnt/mithril-accounts/   # AccountsDB (needs high IOPS - use fastest drive)
/mnt/mithril-ledger/     # Blockstore and snapshots (can use slower drive)
    ├── blockstore/
    └── snapshots/
```

### Current Limitations

- **Block Catchup**: Mithril currently relies on `getBlock` RPC calls to catch up to the tip of mainnet-beta. We are actively working on adding support for direct shred replay, which will be more decentralized and performant.

### Troubleshooting

**Slow snapshot downloads**
- The snapshot finder automatically tests many nodes and selects the fastest. If downloads are consistently slow, your network may be the bottleneck.
- Enable `verbose = true` in the `[snapshot]` config section to see detailed node discovery statistics.

**High disk I/O**
- Ensure AccountsDB is on your fastest NVMe drive
- Consider using a higher-endurance drive (Samsung 990 Pro recommended)

**Out of memory**
- Mithril streams snapshots directly to processing without requiring disk space for the full snapshot file
- However, AccountsDB operations require substantial RAM during initial sync

---

## Development Milestones

### Milestone 1 (Completed): Reimplementation of the Solana Virtual Machine in Golang
- Completed in August 2024, read more [here](https://overclock.one/rnd/unveiling-mithril).
- Reimplementation of all syscalls, with a comprehensive test suite developed and exercised; bugs found as a result fixed.
- Reimplementation of all native programs, with a comprehensive test suite developed and exercised; bugs found as a result fixed.
- Implementation of the remainder of the runtime and VM, with a comprehensive test suite also developed. Any bugs found as a result of testing and review to be fixed.

### Milestone 2 (Completed): Block Replay and Simple RPC Interface
- Snapshot retrieval and loading.
- Full implementation of transaction (and therefore block) handling.
- Incorporate the AccountsDB and Blockstore facilities that are necessary for data storage and retrieval.
- Minimal RPC interface.
- Development and intensive use of a robust and comprehensive 'conformance suite' for verification of compliance of the VM, interpreter, and runtime as a complete unit. Differential fuzzing will be used to detect differences versus relevant versions of the Labs client, and guided fuzzing will be used generally to uncover security and loss-of-availability issues. Any bugs identified during this phase will be remediated.
- **Status**: Mithril can retrieve and replay current MainnetBeta blocks. There are rare bankhash mismatches that we are actively investigating.

### Milestone 3 (In Progress): Alpha Release and System Optimization
- Thorough optimization work on entire system, including on components such as the Virtual Machine and AccountsDB.
- Implementation of block batch processing with configurable block window size.
- Direct shred replay support (alternative to RPC-based block fetching).
- Testing on testnet and devnet environments.
- **Target**: Alpha release in early Q1 2026.

### Future Directions
- Implementation of 'archival node' features including historical replay compatibility.
- gRPC interface support.
- Expanded RPC feature set.
- Ongoing performance improvements.
- We welcome community suggestions - please open an issue or join our Discord to discuss ideas!

---

## Community

- **Discord**: Join the [Overclock Validator Discord](https://discord.gg/overclock) for support and discussion
- **Hardware Discussion**: `#mithril-hardware` channel for hardware recommendations
- **GitHub Issues**: Report bugs and feature requests on the GitHub repository
