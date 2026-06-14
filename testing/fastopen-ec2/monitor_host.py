#!/usr/bin/env python3
"""Sample host CPU and memory usage and emit CSV plus summary JSON."""

from __future__ import annotations

import argparse
import json
import signal
import statistics
import time
from pathlib import Path


running = True


def on_signal(_signum: int, _frame: object) -> None:
    global running
    running = False


def read_proc_stat() -> tuple[int, int]:
    line = Path("/proc/stat").read_text(encoding="utf-8").splitlines()[0]
    parts = line.split()
    values = [int(v) for v in parts[1:]]
    idle = values[3] + values[4]
    total = sum(values)
    return total, idle


def read_meminfo() -> dict[str, int]:
    out: dict[str, int] = {}
    for line in Path("/proc/meminfo").read_text(encoding="utf-8").splitlines():
        key, rest = line.split(":", 1)
        out[key] = int(rest.strip().split()[0]) * 1024
    return out


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


def ensure_parent(path: str) -> None:
    Path(path).parent.mkdir(parents=True, exist_ok=True)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--interval", type=float, default=1.0)
    parser.add_argument("--output-csv", required=True)
    parser.add_argument("--summary-json", required=True)
    args = parser.parse_args()

    signal.signal(signal.SIGTERM, on_signal)
    signal.signal(signal.SIGINT, on_signal)

    ensure_parent(args.output_csv)
    ensure_parent(args.summary_json)

    samples: list[dict[str, float]] = []
    prev_total, prev_idle = read_proc_stat()
    start = time.time()

    with open(args.output_csv, "w", encoding="utf-8") as fh:
        fh.write(
            "timestamp_unix,elapsed_seconds,cpu_percent,mem_used_percent,mem_used_bytes,mem_available_bytes,mem_total_bytes\n"
        )
        while running:
            time.sleep(args.interval)
            now = time.time()
            total, idle = read_proc_stat()
            total_delta = total - prev_total
            idle_delta = idle - prev_idle
            prev_total, prev_idle = total, idle
            cpu_percent = 0.0
            if total_delta > 0:
                cpu_percent = (1.0 - idle_delta / total_delta) * 100.0

            meminfo = read_meminfo()
            total_mem = float(meminfo["MemTotal"])
            avail_mem = float(meminfo.get("MemAvailable", meminfo.get("MemFree", 0)))
            used_mem = total_mem - avail_mem
            mem_used_percent = (used_mem / total_mem) * 100.0 if total_mem else 0.0
            elapsed = now - start

            sample = {
                "timestamp_unix": now,
                "elapsed_seconds": elapsed,
                "cpu_percent": cpu_percent,
                "mem_used_percent": mem_used_percent,
                "mem_used_bytes": used_mem,
                "mem_available_bytes": avail_mem,
                "mem_total_bytes": total_mem,
            }
            samples.append(sample)
            fh.write(
                f"{now:.3f},{elapsed:.3f},{cpu_percent:.3f},{mem_used_percent:.3f},"
                f"{used_mem:.0f},{avail_mem:.0f},{total_mem:.0f}\n"
            )
            fh.flush()

    summary = {
        "sample_interval_seconds": args.interval,
        "sample_count": len(samples),
        "cpu_percent": summarize([s["cpu_percent"] for s in samples]),
        "mem_used_percent": summarize([s["mem_used_percent"] for s in samples]),
        "mem_used_bytes": summarize([s["mem_used_bytes"] for s in samples]),
        "mem_available_bytes": summarize([s["mem_available_bytes"] for s in samples]),
    }
    Path(args.summary_json).write_text(
        json.dumps(summary, indent=2, sort_keys=True), encoding="utf-8"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
