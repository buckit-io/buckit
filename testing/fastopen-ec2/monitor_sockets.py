#!/usr/bin/env python3
"""Sample TCP socket states for Buckit node-to-node traffic."""

from __future__ import annotations

import argparse
import json
import signal
import statistics
import subprocess
import time
from collections import Counter
from pathlib import Path


running = True
STATES = ["ESTAB", "TIME-WAIT", "CLOSE-WAIT", "SYN-SENT", "SYN-RECV", "FIN-WAIT-1", "FIN-WAIT-2", "LAST-ACK", "CLOSING"]


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
        return {"count": 0, "min": None, "max": None, "mean": None, "p50": None, "p90": None, "p99": None}
    return {
        "count": len(values),
        "min": min(values),
        "max": max(values),
        "mean": statistics.mean(values),
        "p50": percentile(values, 50),
        "p90": percentile(values, 90),
        "p99": percentile(values, 99),
    }


def socket_counts(peer_ip: str, port: str) -> Counter[str]:
    result = subprocess.run(
        ["ss", "-tan"],
        check=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    counts: Counter[str] = Counter()
    for line in result.stdout.splitlines()[1:]:
        parts = line.split()
        if len(parts) < 5:
            continue
        state, local, peer = parts[0], parts[3], parts[4]
        if peer_ip not in local and peer_ip not in peer:
            continue
        if not (local.endswith(":" + port) or peer.endswith(":" + port)):
            continue
        counts[state] += 1
        counts["TOTAL"] += 1
    return counts


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--interval", type=float, default=1.0)
    parser.add_argument("--peer-ip", required=True)
    parser.add_argument("--port", default="9000")
    parser.add_argument("--output-csv", required=True)
    parser.add_argument("--summary-json", required=True)
    args = parser.parse_args()

    signal.signal(signal.SIGTERM, on_signal)
    signal.signal(signal.SIGINT, on_signal)

    ensure_parent(args.output_csv)
    ensure_parent(args.summary_json)

    samples: list[dict[str, float]] = []
    start = time.time()
    headers = ["timestamp_unix", "elapsed_seconds", "total", *[state.lower().replace("-", "_") for state in STATES]]
    with open(args.output_csv, "w", encoding="utf-8") as fh:
        fh.write(",".join(headers) + "\n")
        while running:
            time.sleep(args.interval)
            now = time.time()
            counts = socket_counts(args.peer_ip, args.port)
            sample = {
                "timestamp_unix": now,
                "elapsed_seconds": now - start,
                "total": float(counts["TOTAL"]),
            }
            for state in STATES:
                sample[state.lower().replace("-", "_")] = float(counts[state])
            samples.append(sample)
            fh.write(",".join(f"{sample[name]:.3f}" for name in headers) + "\n")
            fh.flush()

    summary = {
        "peer_ip": args.peer_ip,
        "port": args.port,
        "sample_interval_seconds": args.interval,
        "sample_count": len(samples),
    }
    for name in headers[2:]:
        summary[name] = summarize([s[name] for s in samples])
    Path(args.summary_json).write_text(json.dumps(summary, indent=2, sort_keys=True), encoding="utf-8")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
