#!/usr/bin/env python3
"""Sample CPU and memory metrics for a Docker container via cgroup files."""

from __future__ import annotations

import argparse
import csv
import json
import signal
import statistics
import subprocess
import time
from pathlib import Path


running = True


def on_signal(_signum: int, _frame: object) -> None:
    global running
    running = False


def ensure_parent(path: str) -> None:
    Path(path).parent.mkdir(parents=True, exist_ok=True)


def percentile(values: list[float], pct: float) -> float | None:
    if not values:
        return None
    if len(values) == 1:
        return values[0]
    ordered = sorted(values)
    pos = (len(ordered) - 1) * pct / 100.0
    lower = int(pos)
    upper = min(lower + 1, len(ordered) - 1)
    if lower == upper:
        return ordered[lower]
    frac = pos - lower
    return ordered[lower] * (1 - frac) + ordered[upper] * frac


def summarize(values: list[float]) -> dict[str, float | int | None]:
    if not values:
        return {
            "count": 0,
            "min": None,
            "max": None,
            "mean": None,
            "p50": None,
            "p90": None,
            "p99": None,
        }
    return {
        "count": len(values),
        "min": min(values),
        "max": max(values),
        "mean": statistics.mean(values),
        "p50": percentile(values, 50),
        "p90": percentile(values, 90),
        "p99": percentile(values, 99),
    }


def read_container_sample(container: str) -> tuple[int, int, int, int | None, int | None]:
    cmd = (
        "awk '/usage_usec/ {print $2}' /sys/fs/cgroup/cpu.stat; "
        "cat /sys/fs/cgroup/memory.current; "
        "cat /sys/fs/cgroup/memory.max; "
        "cat /sys/fs/cgroup/cpu.max"
    )
    text = subprocess.check_output(
        ["docker", "exec", container, "sh", "-lc", cmd],
        text=True,
    )
    usage_usec_str, mem_current_str, mem_max_str, cpu_max = text.strip().splitlines()
    quota_str, period_str = cpu_max.split()
    quota = None if quota_str == "max" else int(quota_str)
    period = None if quota is None else int(period_str)
    return (
        int(usage_usec_str),
        int(mem_current_str),
        int(mem_max_str),
        quota,
        period,
    )


def cpu_limit_cores(quota: int | None, period: int | None) -> float:
    if quota is None or period is None or period <= 0:
        return 1.0
    return quota / period


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--container", required=True)
    parser.add_argument("--interval", type=float, default=1.0)
    parser.add_argument("--output-csv", required=True)
    parser.add_argument("--summary-json", required=True)
    args = parser.parse_args()

    signal.signal(signal.SIGTERM, on_signal)
    signal.signal(signal.SIGINT, on_signal)

    ensure_parent(args.output_csv)
    ensure_parent(args.summary_json)

    start = time.time()
    prev_time = start
    prev_usage, _, _, quota, period = read_container_sample(args.container)
    cores = cpu_limit_cores(quota, period)
    samples: list[dict[str, float]] = []

    with open(args.output_csv, "w", newline="", encoding="utf-8") as fh:
        writer = csv.writer(fh)
        writer.writerow(
            [
                "timestamp_unix",
                "elapsed_seconds",
                "cpu_percent",
                "mem_used_bytes",
                "mem_limit_bytes",
                "mem_used_percent",
                "cpu_limit_cores",
            ]
        )
        while running:
            time.sleep(args.interval)
            now = time.time()
            usage, mem_current, mem_max, _, _ = read_container_sample(args.container)
            delta_time = now - prev_time
            delta_usage = usage - prev_usage
            cpu_percent = 0.0
            if delta_time > 0 and cores > 0:
                cpu_percent = (delta_usage / (delta_time * 1_000_000.0 * cores)) * 100.0
            mem_used_percent = (mem_current / mem_max) * 100.0 if mem_max > 0 else 0.0
            elapsed = now - start

            sample = {
                "timestamp_unix": now,
                "elapsed_seconds": elapsed,
                "cpu_percent": cpu_percent,
                "mem_used_bytes": float(mem_current),
                "mem_limit_bytes": float(mem_max),
                "mem_used_percent": mem_used_percent,
                "cpu_limit_cores": cores,
            }
            samples.append(sample)
            writer.writerow(
                [
                    f"{now:.3f}",
                    f"{elapsed:.3f}",
                    f"{cpu_percent:.3f}",
                    mem_current,
                    mem_max,
                    f"{mem_used_percent:.3f}",
                    f"{cores:.3f}",
                ]
            )
            fh.flush()
            prev_time = now
            prev_usage = usage

    summary = {
        "container": args.container,
        "sample_interval_seconds": args.interval,
        "sample_count": len(samples),
        "cpu_limit_cores": cores,
        "cpu_percent": summarize([s["cpu_percent"] for s in samples]),
        "mem_used_bytes": summarize([s["mem_used_bytes"] for s in samples]),
        "mem_used_percent": summarize([s["mem_used_percent"] for s in samples]),
    }
    Path(args.summary_json).write_text(
        json.dumps(summary, indent=2, sort_keys=True),
        encoding="utf-8",
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
