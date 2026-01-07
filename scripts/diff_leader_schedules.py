#!/usr/bin/env python3
"""
Diff two leader schedule slot CSVs and report mismatches.

Usage:
    scripts/diff_leader_schedules.py <local_csv> <rpc_csv>

Example:
    scripts/diff_leader_schedules.py \
        /mnt/mithril-logs/leader_schedule_epoch500_local_slots_abc12345.csv \
        /mnt/mithril-logs/leader_schedule_epoch500_rpc_slots_abc12345.csv

Output:
    - Summary of mismatches (count, first/last slot, leader changes)
    - Detailed diff showing up to N mismatches with context

The CSV format expected is:
    slot,leader
    12345678,<base58_pubkey>
    ...
"""

import argparse
import csv
import sys
from collections import defaultdict
from dataclasses import dataclass
from pathlib import Path


@dataclass
class Mismatch:
    slot: int
    local_leader: str
    rpc_leader: str


def load_schedule(filepath: Path) -> dict[int, str]:
    """Load a schedule CSV into {slot: leader} dict."""
    schedule = {}
    with open(filepath, 'r') as f:
        reader = csv.DictReader(f)
        for row in reader:
            slot = int(row['slot'])
            leader = row['leader'].strip()
            schedule[slot] = leader
    return schedule


def find_mismatches(local: dict[int, str], rpc: dict[int, str]) -> list[Mismatch]:
    """Compare two schedules and return list of mismatches."""
    mismatches = []

    # Get all slots from both schedules
    all_slots = sorted(set(local.keys()) | set(rpc.keys()))

    for slot in all_slots:
        local_leader = local.get(slot, '')
        rpc_leader = rpc.get(slot, '')

        if local_leader != rpc_leader:
            mismatches.append(Mismatch(slot, local_leader, rpc_leader))

    return mismatches


def analyze_leader_changes(mismatches: list[Mismatch]) -> dict[tuple[str, str], int]:
    """Count how many times each (local, rpc) leader pair appears in mismatches."""
    counts = defaultdict(int)
    for m in mismatches:
        counts[(m.local_leader, m.rpc_leader)] += 1
    return dict(sorted(counts.items(), key=lambda x: -x[1]))


def find_consecutive_runs(mismatches: list[Mismatch]) -> list[tuple[int, int, int]]:
    """Find runs of consecutive mismatched slots. Returns [(start_slot, end_slot, count), ...]."""
    if not mismatches:
        return []

    runs = []
    run_start = mismatches[0].slot
    run_end = mismatches[0].slot

    for i in range(1, len(mismatches)):
        if mismatches[i].slot == run_end + 1:
            run_end = mismatches[i].slot
        else:
            runs.append((run_start, run_end, run_end - run_start + 1))
            run_start = mismatches[i].slot
            run_end = mismatches[i].slot

    runs.append((run_start, run_end, run_end - run_start + 1))
    return runs


def format_pubkey(pk: str, width: int = 12) -> str:
    """Shorten pubkey for display: first6...last4."""
    if len(pk) <= width:
        return pk.ljust(width)
    return f"{pk[:6]}...{pk[-4:]}"


def main():
    parser = argparse.ArgumentParser(
        description='Diff two leader schedule slot CSVs',
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=__doc__
    )
    parser.add_argument('local_csv', type=Path, help='Local schedule CSV file')
    parser.add_argument('rpc_csv', type=Path, help='RPC schedule CSV file')
    parser.add_argument('-n', '--max-show', type=int, default=20,
                        help='Max individual mismatches to show (default: 20)')
    parser.add_argument('-o', '--out', type=Path, default=None,
                        help='Write full mismatch list to CSV (slot,local,rpc)')
    parser.add_argument('-v', '--verbose', action='store_true',
                        help='Show more details')
    args = parser.parse_args()

    # Load schedules
    print(f"Loading {args.local_csv}...")
    local = load_schedule(args.local_csv)
    print(f"  Loaded {len(local):,} slots")

    print(f"Loading {args.rpc_csv}...")
    rpc = load_schedule(args.rpc_csv)
    print(f"  Loaded {len(rpc):,} slots")

    # Quick sanity check
    if len(local) != len(rpc):
        print(f"\nWARNING: Different slot counts! local={len(local):,} rpc={len(rpc):,}")

    # Find mismatches
    mismatches = find_mismatches(local, rpc)

    if not mismatches:
        print("\n=== MATCH ===")
        print("All slots have identical leaders!")
        sys.exit(0)

    # Write full mismatch list if requested
    if args.out:
        with open(args.out, 'w', newline='') as f:
            writer = csv.writer(f)
            writer.writerow(['slot', 'local', 'rpc'])
            for m in mismatches:
                writer.writerow([m.slot, m.local_leader, m.rpc_leader])
        print(f"\nWrote {len(mismatches):,} mismatches to {args.out}")

    # Summary - use max slot count for percentage when counts differ
    total_slots = max(len(local), len(rpc))
    print(f"\n=== MISMATCH SUMMARY ===")
    print(f"Total mismatches: {len(mismatches):,} / {total_slots:,} slots ({len(mismatches)/total_slots*100:.2f}%)")
    print(f"First mismatch: slot {mismatches[0].slot:,}")
    print(f"Last mismatch:  slot {mismatches[-1].slot:,}")

    # Analyze leader changes
    print(f"\n=== LEADER CHANGE PATTERNS ===")
    leader_changes = analyze_leader_changes(mismatches)
    print(f"Unique (local, rpc) pairs: {len(leader_changes)}")
    print("\nTop 10 leader change pairs:")
    print(f"  {'Count':>6}  {'Local Leader':<14}  {'RPC Leader':<14}")
    print(f"  {'-'*6}  {'-'*14}  {'-'*14}")
    for (local_pk, rpc_pk), count in list(leader_changes.items())[:10]:
        print(f"  {count:>6}  {format_pubkey(local_pk):<14}  {format_pubkey(rpc_pk):<14}")

    # Consecutive runs
    runs = find_consecutive_runs(mismatches)
    print(f"\n=== CONSECUTIVE MISMATCH RUNS ===")
    print(f"Total runs: {len(runs)}")
    if runs:
        longest_run = max(runs, key=lambda x: x[2])
        print(f"Longest run: {longest_run[2]} slots (slots {longest_run[0]:,}-{longest_run[1]:,})")

        # Show runs of 4+ consecutive mismatches (leader rotation boundary)
        big_runs = [r for r in runs if r[2] >= 4]
        if big_runs:
            print(f"\nRuns of 4+ consecutive mismatches ({len(big_runs)}):")
            for start, end, count in big_runs[:10]:
                print(f"  Slots {start:,}-{end:,} ({count} slots)")
            if len(big_runs) > 10:
                print(f"  ... and {len(big_runs) - 10} more")

    # Show individual mismatches
    print(f"\n=== INDIVIDUAL MISMATCHES (showing first {args.max_show}) ===")
    print(f"  {'Slot':>12}  {'Local Leader':<14}  {'RPC Leader':<14}")
    print(f"  {'-'*12}  {'-'*14}  {'-'*14}")
    for m in mismatches[:args.max_show]:
        print(f"  {m.slot:>12,}  {format_pubkey(m.local_leader):<14}  {format_pubkey(m.rpc_leader):<14}")

    if len(mismatches) > args.max_show:
        print(f"  ... and {len(mismatches) - args.max_show:,} more")

    # Verbose: show last few as well
    if args.verbose and len(mismatches) > args.max_show * 2:
        print(f"\nLast {args.max_show} mismatches:")
        print(f"  {'Slot':>12}  {'Local Leader':<14}  {'RPC Leader':<14}")
        print(f"  {'-'*12}  {'-'*14}  {'-'*14}")
        for m in mismatches[-args.max_show:]:
            print(f"  {m.slot:>12,}  {format_pubkey(m.local_leader):<14}  {format_pubkey(m.rpc_leader):<14}")

    print(f"\n=== RESULT: MISMATCH ===")
    print(f"{len(mismatches):,} slots differ between local and RPC schedules")
    sys.exit(1)


if __name__ == '__main__':
    main()
