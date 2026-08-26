# Troubleshooting Guide

## Clean Shutdown

Always use `Ctrl+C` to stop Mithril cleanly rather than killing the terminal or closing the SSH session. This allows Mithril to flush data and exit gracefully, preventing data corruption and ensuring file handles are properly released.

## Live Processes and Phantom Disk Usage

A live or stopped process can keep a deleted file open, making `df` report more
used space than `du`. A zombie cannot hold file descriptors. Mithril takes a
lifetime lock inside its AccountsDB root and refuses a second writer; startup
never kills another process.

**Symptoms:**
- `df -h` shows disk at 100% usage
- `du -sh /path/to/data` shows significantly less data than expected
- SIGBUS errors during AccountsDB flush operations

**Prevention:**
- Always use `Ctrl+C` to stop Mithril cleanly
- Run each node with a distinct AccountsDB root
- Keep the service supervisor's stop timeout long enough for clean shutdown

**Recovery:**
1. List exact processes and open deleted files with `pgrep -ax mithril` and
   `sudo lsof +L1`. Do not use `pkill -f`.
2. Verify the exact PID, executable, configuration, and storage root. Send that
   PID `SIGINT` and wait for it to exit. Escalate to a stronger signal only
   after recording why clean shutdown failed; never target unrelated nodes.
3. Inspect the retained state before changing storage:
   ```bash
   mithril state --accounts /absolute/accounts/root show
   mithril state --accounts /absolute/accounts/root history
   mithril state --accounts /absolute/accounts/root validate
   ```
4. If startup reports a checkpoint-valid retained fold boundary, run the node
   once with its normal configuration plus `--rewind-to-slot SLOT`. If no
   retained boundary validates, stop. Rename or copy the old root for review,
   configure a distinct empty AccountsDB root, and only then use
   `--bootstrap snapshot`; snapshot bootstrap cleans its configured root.
5. Unmount only after every process using the exact mount has stopped. Confirm
   the source and target with `findmnt`; do not use a lazy unmount or assume a
   device name.

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
