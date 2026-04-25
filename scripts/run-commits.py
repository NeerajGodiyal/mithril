import argparse
import datetime
import os
import re
import statistics
import subprocess
import sys


def build_commit(commit):
    short = subprocess.check_output(
        ["git", "rev-parse", "--short", commit], text=True
    ).strip()
    binary = f"./mithril-{short}"
    subprocess.run(["git", "checkout", commit], check=True)
    print(f">>> building {short}...")
    subprocess.run(["go", "build", "-o", binary, "./cmd/mithril"], check=True)
    return short, binary


def run_commit(short, binary, snapshot, num_slots, config):
    date_str = datetime.date.today().strftime("%Y%m%d")
    log_name = f"mithril-{date_str}-{short}.log"

    cmd = [
        binary, "run",
        "--snapshot", snapshot,
        "--num-slots", str(num_slots),
        "--config", config,
    ]

    print(f">>> {' '.join(cmd)}")
    print(f">>> logging to {log_name}")

    with open(log_name, "w") as log:
        proc = subprocess.Popen(
            cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT
        )
        for line in proc.stdout:
            sys.stdout.buffer.write(line)
            sys.stdout.buffer.flush()
            log.write(line.decode())
        proc.wait()

    return proc.returncode, log_name


def main():
    p = argparse.ArgumentParser(description="Build and run mithril at multiple commits")
    p.add_argument("commits", nargs="+", help="git commits to run")
    p.add_argument("--snapshot", required=True)
    p.add_argument("--num-slots", type=int, default=1000)
    p.add_argument("--config", default="config.toml")
    args = p.parse_args()

    errors = []
    if not os.path.exists(args.snapshot):
        errors.append(f"snapshot not found: {args.snapshot}")
    if not os.path.exists(args.config):
        errors.append(f"config not found: {args.config}")
    if errors:
        p.error("\n  ".join(errors))

    orig_branch = subprocess.check_output(
        ["git", "rev-parse", "--abbrev-ref", "HEAD"], text=True
    ).strip()

    try:
        builds = []
        for commit in args.commits:
            short, binary = build_commit(commit)
            builds.append((short, binary))
        print(">>> all commits built ok")
    finally:
        subprocess.run(["git", "checkout", orig_branch], check=True)

    log_paths = []
    for short, binary in builds:
        rc, log_path = run_commit(short, binary, args.snapshot, args.num_slots, args.config)
        log_paths.append(log_path)
        if rc != 0:
            print(f">>> {short} exited with {rc}, continuing...")

    print()
    print(">>> summary:")
    for log_path in log_paths:
        summarize(log_path, load_vals(log_path))


def parse_exec(line):
    m = re.search(r'exec:\s+([0-9.]+)s', line)
    if m:
        return float(m.group(1))
    return None


def load_vals(path):
    vals = []
    with open(path) as f:
        for line in f:
            if (v := parse_exec(line)) is not None:
                vals.append(v)
    return sorted(vals[1:])  # drop first timing


def summarize(name, vals):
    n = len(vals)
    if n == 0:
        print(f'{name}: no data')
        return
    pct = statistics.quantiles(vals, n=100, method='inclusive')
    print(f'{name}:')
    print(f'  n={n} mean={statistics.mean(vals):.4f} p10={pct[9]:.4f} p25={pct[24]:.4f} p50={pct[49]:.4f} p75={pct[74]:.4f} p90={pct[89]:.4f}')


if __name__ == "__main__":
    main()
