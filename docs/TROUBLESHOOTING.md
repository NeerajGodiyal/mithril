# Troubleshooting Guide

## Clean Shutdown

Always use `Ctrl+C` to stop Mithril cleanly rather than killing the terminal or closing the SSH session. This allows Mithril to flush data and exit gracefully, preventing data corruption and ensuring file handles are properly released.

## Zombie Processes and Phantom Disk Usage

If Mithril processes are terminated abruptly (e.g., by closing SSH or killing the terminal), they may become zombie processes that hold deleted files open. This can cause "phantom" disk usage where `df` shows the disk as full but `du` shows less data than expected.

**Symptoms:**
- `df -h` shows disk at 100% usage
- `du -sh /path/to/data` shows significantly less data than expected
- SIGBUS errors during AccountsDB flush operations

**Prevention:**
- Mithril automatically detects and kills existing mithril processes at startup
- Always use `Ctrl+C` to stop Mithril cleanly

**Recovery:**
1. Check for zombie mithril processes: `ps aux | grep mithril`
2. Kill any stopped/zombie processes: `pkill -9 -f mithril`
3. Unmount and remount the affected disk to release file handles:
   ```bash
   sudo umount -l /mnt/mithril-accounts
   sudo mount /dev/nvme1n1p1 /mnt/mithril-accounts
   ```

## Slow Snapshot Downloads

The snapshot finder automatically tests many nodes and selects the fastest. If downloads are consistently slow:

1. Check your network bandwidth
2. Try increasing stage 2 parameters in config
3. Enable `verbose = true` in the `[snapshot]` config section to see detailed node discovery statistics

## High Disk I/O

- Ensure AccountsDB is on your fastest NVMe drive
- Consider using a higher-endurance drive (Samsung 990 Pro or better recommended)

## Out of Memory

- By default, Mithril saves snapshots to disk (`max_full_snapshots = 1`). Set `max_full_snapshots = 0` for stream-only mode which doesn't require disk space for snapshot files.
- Initial sync uses more RAM than steady-state replay
- Consider increasing swap space for systems with limited RAM
