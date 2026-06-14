#!/usr/bin/env python3
"""Sample Go runtime/GC metrics from a Prometheus endpoint."""

from __future__ import annotations

import argparse
import json
import signal
import statistics
import time
import urllib.request
from pathlib import Path


running = True

METRIC_NAMES = [
    "go_gc_duration_seconds_sum",
    "go_gc_duration_seconds_count",
    "go_memstats_gc_cpu_fraction",
    "go_memstats_last_gc_time_seconds",
    "go_memstats_heap_alloc_bytes",
    "go_memstats_heap_inuse_bytes",
    "go_memstats_next_gc_bytes",
    "go_goroutines",
]


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


def scrape_metrics(url: str, timeout: float) -> dict[str, float]:
    with urllib.request.urlopen(url, timeout=timeout) as resp:
        text = resp.read().decode("utf-8", errors="replace")
    out: dict[str, float] = {}
    for line in text.splitlines():
        if not line or line.startswith("#"):
            continue
        metric_id, _, value_str = line.rpartition(" ")
        name = metric_id.split("{", 1)[0]
        if name not in METRIC_NAMES:
            continue
        try:
            value = float(value_str)
        except ValueError:
            continue
        out[name] = value
    return out


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--interval", type=float, default=1.0)
    parser.add_argument("--timeout", type=float, default=5.0)
    parser.add_argument("--url", required=True)
    parser.add_argument("--output-csv", required=True)
    parser.add_argument("--summary-json", required=True)
    args = parser.parse_args()

    signal.signal(signal.SIGTERM, on_signal)
    signal.signal(signal.SIGINT, on_signal)

    ensure_parent(args.output_csv)
    ensure_parent(args.summary_json)

    samples: list[dict[str, float]] = []
    start = time.time()

    headers = [
        "timestamp_unix",
        "elapsed_seconds",
        *METRIC_NAMES,
        "gc_pause_sum_delta",
        "gc_count_delta",
        "gc_pause_avg_seconds",
    ]
    prev: dict[str, float] | None = None

    with open(args.output_csv, "w", encoding="utf-8") as fh:
        fh.write(",".join(headers) + "\n")
        while running:
            time.sleep(args.interval)
            now = time.time()
            metrics = scrape_metrics(args.url, args.timeout)
            elapsed = now - start

            pause_sum_delta = 0.0
            gc_count_delta = 0.0
            gc_pause_avg = 0.0
            if prev is not None:
                pause_sum_delta = (
                    metrics.get("go_gc_duration_seconds_sum", 0.0)
                    - prev.get("go_gc_duration_seconds_sum", 0.0)
                )
                gc_count_delta = (
                    metrics.get("go_gc_duration_seconds_count", 0.0)
                    - prev.get("go_gc_duration_seconds_count", 0.0)
                )
                if gc_count_delta > 0:
                    gc_pause_avg = pause_sum_delta / gc_count_delta
            prev = metrics

            sample = {
                "timestamp_unix": now,
                "elapsed_seconds": elapsed,
                **{name: metrics.get(name, 0.0) for name in METRIC_NAMES},
                "gc_pause_sum_delta": pause_sum_delta,
                "gc_count_delta": gc_count_delta,
                "gc_pause_avg_seconds": gc_pause_avg,
            }
            samples.append(sample)
            fh.write(
                ",".join(
                    [
                        f"{sample['timestamp_unix']:.3f}",
                        f"{sample['elapsed_seconds']:.3f}",
                        *[
                            f"{sample[name]:.9f}"
                            for name in METRIC_NAMES
                        ],
                        f"{sample['gc_pause_sum_delta']:.9f}",
                        f"{sample['gc_count_delta']:.9f}",
                        f"{sample['gc_pause_avg_seconds']:.9f}",
                    ]
                )
                + "\n"
            )
            fh.flush()

    summary = {
        "sample_interval_seconds": args.interval,
        "sample_count": len(samples),
        "url": args.url,
        "gc_pause_sum_delta": summarize([s["gc_pause_sum_delta"] for s in samples]),
        "gc_count_delta": summarize([s["gc_count_delta"] for s in samples]),
        "gc_pause_avg_seconds": summarize(
            [s["gc_pause_avg_seconds"] for s in samples if s["gc_count_delta"] > 0]
        ),
        "go_memstats_gc_cpu_fraction": summarize(
            [s["go_memstats_gc_cpu_fraction"] for s in samples]
        ),
        "go_memstats_heap_alloc_bytes": summarize(
            [s["go_memstats_heap_alloc_bytes"] for s in samples]
        ),
        "go_memstats_heap_inuse_bytes": summarize(
            [s["go_memstats_heap_inuse_bytes"] for s in samples]
        ),
        "go_memstats_next_gc_bytes": summarize(
            [s["go_memstats_next_gc_bytes"] for s in samples]
        ),
        "go_goroutines": summarize([s["go_goroutines"] for s in samples]),
        "go_gc_duration_seconds_sum_total_delta": 0.0,
        "go_gc_duration_seconds_count_total_delta": 0.0,
    }
    if samples:
        first = samples[0]
        last = samples[-1]
        summary["go_gc_duration_seconds_sum_total_delta"] = (
            last["go_gc_duration_seconds_sum"] - first["go_gc_duration_seconds_sum"]
        )
        summary["go_gc_duration_seconds_count_total_delta"] = (
            last["go_gc_duration_seconds_count"] - first["go_gc_duration_seconds_count"]
        )

    Path(args.summary_json).write_text(
        json.dumps(summary, indent=2, sort_keys=True), encoding="utf-8"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
