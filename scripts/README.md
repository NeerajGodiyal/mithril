# Mithril System Setup Guide

This guide walks you through setting up your Ubuntu system to run Mithril optimally. These scripts are **optional** but recommended for best performance.

> **New to Linux?** Don't worry! This guide explains each step in detail. If you get stuck, join our [Discord](https://discord.gg/overclock) and ask in `#mithril-hardware`.

> **Note:** All commands in this guide assume you're in the mithril repository root directory (e.g., `~/mithril`). If you cloned elsewhere, adjust paths accordingly.

---

## Table of Contents

1. [Before You Start](#before-you-start)
2. [Server Setup Script](#server-setup-script) (fresh OS install or hardening)
3. [Understanding Mithril's Storage Needs](#understanding-mithrils-storage-needs)
4. [Disk Setup Script](#disk-setup-script)
5. [Performance Tuning Script](#performance-tuning-script)
6. [Resetting Mithril Data](#resetting-mithril-data)
7. [Troubleshooting](#troubleshooting)

---

## Before You Start

### What You'll Need

- **Ubuntu 24.04 LTS** (fresh install recommended)
- **At least one NVMe SSD** (1 TB or larger)
- **Terminal access** (Ctrl+Alt+T opens a terminal)
- **sudo privileges** (you can run admin commands)

### Opening a Terminal

1. Press `Ctrl + Alt + T` on your keyboard
2. A black window with white text should appear - this is your terminal
3. You type commands here and press Enter to run them

### What is `sudo`?

`sudo` means "run this command as administrator". When you see `sudo` at the start of a command, Linux will ask for your password. This is normal and required for system changes.

```bash
# Example: this command needs admin privileges
sudo ./scripts/disk-setup.sh --benchmark
```

When you type your password, **you won't see any characters appear** - this is a security feature. Just type your password and press Enter.

---

## Server Setup Script

The server setup script helps you install Ubuntu from scratch or harden an existing installation. This is useful for dedicated servers, bare metal, or VPS providers that offer rescue/live environments.

### When to Use This Script

**Use `install` mode if:**
- You have a bare metal server booted into rescue/live mode
- You want a clean Ubuntu 24.04 installation optimized for Mithril
- Your provider doesn't offer Ubuntu 24.04 directly

**Use `harden` mode if:**
- You already have Ubuntu installed
- You want to add SSH key authentication
- You want to configure security packages (fail2ban, UFW, etc.)

**Use `status` mode to:**
- Check current security configuration
- Verify SSH settings and firewall status

### Install Mode (Fresh OS Install)

> ⚠️ **WARNING**: Install mode **erases the OS disk** you select. Only use in rescue/live environments on drives you want to dedicate to the OS.

Boot your server into rescue mode (via your provider's panel), then:

```bash
# Download the script (or clone the repo)
curl -O https://raw.githubusercontent.com/Overclock-Validator/mithril/main/scripts/server-setup.sh
chmod +x server-setup.sh

sudo ./server-setup.sh install
```

The script will:
1. Ask which disk to install Ubuntu on
2. Create a fixed-size root partition (not consuming entire disk)
3. Optionally create a data partition with remaining space
4. Set up an admin user with your SSH key
5. Configure security: fail2ban, UFW, unattended-upgrades
6. Disable SSH password login (key-only)
7. Install chrony (time sync) and haveged (entropy)
8. Set journald limits to prevent log disk fill

After installation, disable rescue boot in your provider panel and reboot.

### Harden Mode (Existing Ubuntu)

Safe to run on an existing Ubuntu system:

```bash
sudo ./scripts/server-setup.sh harden
```

This mode will:
1. Create/ensure an admin user with sudo access
2. Help you add an SSH public key
3. Optionally disable SSH password login
4. Install and configure fail2ban, UFW, unattended-upgrades

### Status Mode

Check current security configuration (no changes made):

```bash
./scripts/server-setup.sh status
```

Shows SSH settings, fail2ban status, UFW rules, and more.

### What Gets Configured

| Component | What It Does |
|-----------|--------------|
| **SSH hardening** | Disable password login, key-only access |
| **fail2ban** | Blocks IPs after failed SSH attempts |
| **UFW firewall** | Default deny incoming, allow SSH |
| **unattended-upgrades** | Auto-install security patches |
| **chrony** | Time synchronization (critical for blockchain) |
| **haveged** | Entropy generation for cryptographic operations |
| **journald limits** | Prevents logs from filling disk (2GB max) |

---

## Understanding Mithril's Storage Needs

Before running the scripts, it helps to understand what Mithril stores and why it matters.

### What Mithril Stores

Mithril stores three types of data:

| Storage Type | What It Is | Size | Speed Needed |
|-------------|------------|------|--------------|
| **AccountsDB** | All Solana account balances and data | ~500 GB (reserve 700 GB) | **Very fast** (random reads) |
| **Snapshots** | Downloaded network state | ~100 GB | Moderate (streaming) |
| **Blockstore** | Verified blocks | Configurable | Moderate (sequential writes) |

### Why AccountsDB Needs Your Fastest Drive

When Mithril verifies blocks, it constantly looks up account data. These lookups are **random** - jumping around the drive millions of times per second. A fast NVMe with low latency makes a huge difference here.

Snapshots and blockstore are different - they read/write in order (sequentially), which almost any modern SSD handles well.

### Single Drive vs Two Drives

**If you have ONE NVMe drive:**
- Put everything on it
- No special partitioning needed
- The scripts will set this up for you

**If you have TWO NVMe drives:**
- Put AccountsDB on the **faster** one
- Put snapshots/blockstore on the second one
- The benchmark command helps identify which is faster

### Do Separate Partitions Help?

Separate partitions on the same drive do **NOT** improve I/O performance - they still share the same physical hardware. You need separate **physical** drives for actual I/O isolation.

However, partitions do provide operational benefits:
- Prevent disk-full issues in one area from crashing others
- Allow reformatting just AccountsDB without touching your OS
- Enable different mount options per partition (e.g., noatime)

### SSD Over-Provisioning

This is important for SSD longevity and consistent performance.

**What is Over-Provisioning?**
Leave 15-20% of your SSD unallocated (no partition at all). This gives the SSD controller guaranteed "scratch space" for garbage collection and wear leveling. SSD performance degrades significantly when drives exceed 80% capacity.

**Static vs Dynamic Over-Provisioning:**
- **Static OP** (unallocated disk space): The controller always knows these blocks are free. Best performance.
- **Dynamic OP** (free space within partitions): Relies on TRIM commands to tell the controller what's free. Less reliable.

**Recommended unallocated space:**
| Drive Size | Leave Unallocated |
|------------|-------------------|
| 1 TB | 150-200 GB |
| 2 TB | 300-400 GB |

### Recommended Partition Layout (Fresh Install)

If you're doing a fresh Ubuntu installation and want to optimize for Mithril, here's a recommended layout.

**Single Drive Setup (1 TB):**

| Partition | Size | Filesystem | Purpose |
|-----------|------|------------|---------|
| EFI | 512 MB | FAT32 | Boot (required) |
| Ubuntu OS | 30-50 GB | ext4 | Operating system |
| Mithril Data | 650-700 GB | ext4 | AccountsDB, snapshots, blockstore |
| (unallocated) | 150-200 GB | - | Over-provisioning |

**Two Drive Setup:**

*Drive 1 (faster NVMe) - dedicated to AccountsDB:*
| Partition | Size | Filesystem | Purpose |
|-----------|------|------------|---------|
| AccountsDB | 80% of drive | ext4 | AccountsDB only |
| (unallocated) | 20% of drive | - | Over-provisioning |

*Drive 2 (can be slower) - OS and other data:*
| Partition | Size | Filesystem | Purpose |
|-----------|------|------------|---------|
| EFI | 512 MB | FAT32 | Boot (required) |
| Ubuntu OS | 30-50 GB | ext4 | Operating system |
| Snapshots/Blockstore | Remaining | ext4 or xfs | Snapshots and blocks |

**Tips for Ubuntu Installer:**
1. Choose "Something else" (manual partitioning) during installation
2. Create the EFI partition first (if UEFI boot)
3. Create the OS partition and set mount point to `/`
4. Create the Mithril data partition and set mount point (e.g., `/mnt/mithril`)
5. Leave the remaining space unallocated (do not create a partition)

### Getting Disk Info for Manual Setup

To see your disk UUIDs and device info (useful for `/etc/fstab`):

```bash
./scripts/disk-setup.sh --disk-info
```

This shows:
- All NVMe drives with model and serial numbers
- Existing partitions with their UUIDs
- Example fstab entries for mounting

---

## Disk Setup Script

The disk setup script helps you:
- Find your fastest NVMe drive
- Format drives with optimal settings
- Create the directory structure Mithril needs

### Checking Your Drives (Safe - No Changes Made)

First, let's see what drives you have and how fast they are:

```bash
sudo ./scripts/disk-setup.sh --benchmark
```

This command:
1. Lists all your NVMe drives
2. Shows the model and size of each
3. Runs a random 4K read benchmark (~15 seconds per drive)
4. Recommends which drive to use for AccountsDB

**Why random 4K reads?** AccountsDB constantly looks up account data in a random access pattern. Sequential read speed (what most benchmarks show) is not a good predictor of AccountsDB performance. Random 4K IOPS is the metric that matters.

**Required tool:** The benchmark uses `fio` for accurate random I/O testing. Install it with:
```bash
sudo apt install fio
```

**Example output:**
```
  ┌─────────────────────────────────────────────────────────────────────────┐
  │ BENCHMARK RESULTS (random 4K    read)                                   │
  ├─────────────────────────────────────────────────────────────────────────┤
  │ DEVICE        SPEED         SIZE      MODEL                             │
  ├─────────────────────────────────────────────────────────────────────────┤
  │ /dev/nvme0n1  185.3 K IOPS  1.8T      Samsung SSD 990 PRO               │
  │ /dev/nvme1n1   95.7 K IOPS  2.0T      WD Blue SN580                     │
  └─────────────────────────────────────────────────────────────────────────┘

  ✓ Recommended for AccountsDB (fastest): /dev/nvme0n1

  Random 4K IOPS is the key metric for AccountsDB performance.
  Higher IOPS = faster block verification.
```

If `fio` is not installed, the script falls back to `hdparm` sequential read tests (less accurate but better than nothing).

### Viewing Current Storage Status

To see how your drives are currently configured:

```bash
./scripts/disk-setup.sh --status
```

This shows mounted drives, filesystems, and available space.

### Setting Up Drives (Formats Drives - Erases Data!)

> ⚠️ **WARNING**: This will **erase all data** on the drives you select. Only use on drives you want to dedicate to Mithril.

```bash
sudo ./scripts/disk-setup.sh --setup
```

The script will:
1. Offer to run benchmarks first (recommended)
2. Ask which drive to use for AccountsDB
3. Ask which drive for snapshots/blockstore (if you have multiple)
4. Ask which filesystem to use (ext4 or xfs)
5. Show a summary of what will happen
6. Ask you to type a confirmation phrase before making changes

### Choosing a Filesystem: ext4 vs xfs

The script will ask you to choose between ext4 and xfs. Here's what you need to know:

**ext4** (Recommended for most users)
- More mature, widely tested
- Better for AccountsDB's random read/write pattern
- Can shrink partitions if needed later
- The safer default choice

**xfs**
- Can be faster for large sequential operations
- Good for snapshots/blockstore workloads
- Cannot shrink partitions (would need to reformat)
- Faster crash recovery

**Our recommendation:**
- Single drive: Use ext4
- Two drives: ext4 for AccountsDB, xfs for snapshots/blockstore is reasonable
- When in doubt: ext4

We're still benchmarking to determine optimal configurations. Join `#mithril-hardware` on Discord for updates.

---

## Performance Tuning Script

The performance tuning script optimizes your Ubuntu system for running Mithril. All changes are optional and explained in detail.

### Checking Current System Settings

See what's currently configured (no changes made):

```bash
./scripts/performance-tune.sh --status
```

This shows your current kernel settings, CPU mode, I/O scheduler, and more.

### Running Interactively (Recommended for Beginners)

The safest way to run the script - it explains each optimization and asks before applying:

```bash
sudo ./scripts/performance-tune.sh
```

For each optimization, you'll see:
1. **What it does** - Plain English explanation
2. **Why it helps** - How it benefits Mithril
3. **Yes/No prompt** - You choose whether to apply it

### Applying All Optimizations at Once

If you want everything without prompts:

```bash
sudo ./scripts/performance-tune.sh --all
```

### Preview Mode (Dry Run)

See what would happen without making any changes:

```bash
sudo ./scripts/performance-tune.sh --all --dry-run
```

### What Gets Optimized

#### Basic Optimizations (Low Risk)

| Optimization | What It Does | Why It Helps |
|-------------|--------------|--------------|
| **SSD TRIM** | Weekly cleanup of deleted blocks | Extends NVMe lifespan |
| **Kernel tuning** | Adjusts memory/network settings | Better resource management |
| **CPU performance** | Disables power saving throttling | Consistent speed, no slowdowns |
| **noatime** | Stops tracking file access times | Reduces unnecessary disk writes |

#### Advanced Optimizations (Some Risk)

| Optimization | What It Does | Why It Helps |
|-------------|--------------|--------------|
| **I/O scheduler** | Changes how disk requests are ordered | Lower latency for AccountsDB |
| **Read-ahead** | Controls how much data is prefetched | Tuned per-drive for workload |
| **Huge Pages** | Uses larger memory pages | Can improve performance |

### Go Runtime Tips

Mithril is written in Go. The script can show you Go-specific optimizations:

```bash
./scripts/performance-tune.sh --go-tuning
```

This displays:
- Environment variables for garbage collection tuning
- Build-time CPU optimizations (GOAMD64)
- Diagnostic commands for latency issues

---

## Resetting Mithril Data

Sometimes you need to start fresh. These commands delete Mithril data so you can re-sync from scratch.

### Delete AccountsDB Only

Forces Mithril to rebuild from a fresh snapshot on next run:

```bash
sudo ./scripts/disk-setup.sh --delete-accountsdb
```

**When to use:** AccountsDB got corrupted, or you want a clean slate without re-downloading snapshots.

### Delete Snapshots Only

Removes downloaded snapshots (Mithril will re-download them):

```bash
sudo ./scripts/disk-setup.sh --delete-snapshots
```

**When to use:** Snapshots are old or corrupted.

### Delete Blockstore Only

Removes stored blocks (Mithril will rebuild them):

```bash
sudo ./scripts/disk-setup.sh --delete-blockstore
```

**When to use:** You want to clear block history but keep AccountsDB.

### Delete Everything

Complete reset - removes all Mithril data:

```bash
sudo ./scripts/disk-setup.sh --delete-all
```

**When to use:** Starting completely fresh, or decommissioning the node.

### Safety Features

All delete commands:
- Show you exactly what will be deleted and how much space
- Require you to type a confirmation phrase
- Recreate empty directories after deletion

---

## Troubleshooting

### "Permission denied" errors

You need to run with `sudo`:
```bash
sudo ./scripts/disk-setup.sh --benchmark
```

### "Command not found" errors

Make sure you're in the mithril directory:
```bash
cd ~/mithril
```

### Script shows "No NVMe devices found"

- Your drives might not be NVMe (SATA SSDs won't appear)
- Check with: `lsblk` to see all block devices

### Can't format a drive - "has mounted partitions"

Unmount the drive first:
```bash
sudo umount /dev/nvme0n1p1
```

Or if it's in use, you may need to reboot and run the script before the drive gets auto-mounted.

### Benchmark shows low speeds

- Make sure the drive isn't being heavily used during the test
- Check for firmware updates for your NVMe
- Some drives throttle when hot - check temperatures with: `sudo nvme smart-log /dev/nvme0n1`

### Changes didn't persist after reboot

The scripts create systemd services to persist changes. Check if they're enabled:
```bash
systemctl is-enabled mithril-cpu-performance.service
systemctl is-enabled mithril-io-scheduler.service
systemctl is-enabled mithril-readahead.service
```

If any show "disabled", the performance-tune script may not have completed successfully. Try running it again.

---

## Getting Help

- **Discord**: Join [Overclock Validator Discord](https://discord.gg/overclock)
- **Hardware Channel**: `#mithril-hardware` for setup questions
- **GitHub Issues**: Report bugs at the GitHub repository

---

## Quick Reference

```bash
# Server Setup (OS install and security hardening)
sudo ./scripts/server-setup.sh install           # Fresh Ubuntu install (DESTRUCTIVE)
sudo ./scripts/server-setup.sh harden            # Harden existing Ubuntu (safe)
./scripts/server-setup.sh status                 # Show security status

# Disk Setup
sudo ./scripts/disk-setup.sh --benchmark         # Test drive speeds (safe)
sudo ./scripts/disk-setup.sh --setup             # Format and configure drives
./scripts/disk-setup.sh --status                 # Show current configuration
./scripts/disk-setup.sh --disk-info              # Show UUIDs and device info

# Performance Tuning
sudo ./scripts/performance-tune.sh               # Interactive mode
sudo ./scripts/performance-tune.sh --all         # Apply all optimizations
sudo ./scripts/performance-tune.sh --status      # Show current settings
./scripts/performance-tune.sh --go-tuning        # Go runtime tips

# Reset Data
sudo ./scripts/disk-setup.sh --delete-accountsdb # Delete AccountsDB
sudo ./scripts/disk-setup.sh --delete-snapshots  # Delete snapshots
sudo ./scripts/disk-setup.sh --delete-blockstore # Delete blockstore
sudo ./scripts/disk-setup.sh --delete-all        # Delete everything
```
