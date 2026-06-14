#!/usr/bin/env python3
"""Run a mixed GET/PUT/DELETE replay against an existing S3 bucket."""

from __future__ import annotations

import argparse
import concurrent.futures
import csv
import datetime as dt
import json
import math
import os
import random
import statistics
import threading
import time
from collections import Counter
from dataclasses import dataclass
from pathlib import Path

import boto3
from botocore.config import Config


UTC = getattr(dt, "UTC", dt.timezone.utc)


@dataclass(frozen=True)
class ConfigData:
    endpoint: str
    bucket: str
    access_key: str
    secret_key: str
    region: str
    concurrency: int
    duration: float
    get_weight: float
    put_weight: float
    delete_weight: float
    size_hist: str
    put_prefix: str
    output_csv: str
    summary_json: str
    metrics_before: str
    metrics_after: str
    metrics_url: str
    seed: int
    run_id: str
    host_label: str
    report_interval: float
    metrics_sample_url: str
    delay_min_ms: float
    delay_max_ms: float


@dataclass
class Result:
    op: str
    key: str
    status: int
    bytes_read: int
    object_bytes: int
    ttfb_ms: float
    total_ms: float
    error: str
    start_ts: float
    end_ts: float


class KeyPool:
    def __init__(self, read_keys: list[str], put_prefix: str):
        self._read_keys = tuple(read_keys)
        self._mutable_sizes: dict[str, int] = {}
        self._lock = threading.Lock()
        self._next_index = 1
        self._put_prefix = put_prefix

    def random_current(self, rng: random.Random) -> str | None:
        with self._lock:
            if not self._read_keys and not self._mutable_sizes:
                return None
            total = len(self._read_keys) + len(self._mutable_sizes)
            pick = rng.randrange(total)
            if pick < len(self._read_keys):
                return self._read_keys[pick]
            return rng.choice(tuple(self._mutable_sizes))

    def reserve_delete(self, rng: random.Random, target_bytes: int | None = None) -> tuple[str, int] | None:
        with self._lock:
            if not self._mutable_sizes:
                return None
            if target_bytes and target_bytes > 0:
                key = min(
                    self._mutable_sizes,
                    key=lambda k: (abs(self._mutable_sizes[k] - target_bytes), rng.random()),
                )
            else:
                key = rng.choice(tuple(self._mutable_sizes))
            size = self._mutable_sizes.pop(key)
            return key, size

    def restore(self, key: str, size: int) -> None:
        with self._lock:
            self._mutable_sizes[key] = size

    def new_put_key(self) -> str:
        with self._lock:
            key = f"{self._put_prefix}{self._next_index:08d}"
            self._next_index += 1
            return key

    def commit_put(self, key: str, size: int) -> None:
        with self._lock:
            self._mutable_sizes[key] = size

    def snapshot_size(self) -> int:
        with self._lock:
            return len(self._read_keys) + len(self._mutable_sizes)

    def snapshot_mutable_size(self) -> int:
        with self._lock:
            return len(self._mutable_sizes)

    def snapshot_read_size(self) -> int:
        return len(self._read_keys)


class ByteBalance:
    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._put_bytes = 0
        self._delete_bytes = 0

    def note(self, op: str, object_bytes: int) -> None:
        with self._lock:
            if op == "PUT":
                self._put_bytes += object_bytes
            elif op == "DELETE":
                self._delete_bytes += object_bytes

    def snapshot(self) -> tuple[int, int]:
        with self._lock:
            return self._put_bytes, self._delete_bytes

    def reset(self) -> tuple[int, int]:
        with self._lock:
            current = (self._put_bytes, self._delete_bytes)
            self._put_bytes = 0
            self._delete_bytes = 0
            return current


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--endpoint", required=True)
    p.add_argument("--bucket", required=True)
    p.add_argument("--access-key", default="buckitadmin")
    p.add_argument("--secret-key", default="buckitadmin")
    p.add_argument("--region", default="us-east-1")
    p.add_argument("--keys-file", required=True)
    p.add_argument("--concurrency", type=int, default=20)
    p.add_argument("--duration", default="1h", help="Examples: 10m, 1h, 12h")
    p.add_argument("--get-weight", type=float, default=88.0)
    p.add_argument("--put-weight", type=float, default=1.0)
    p.add_argument("--delete-weight", type=float, default=1.0)
    p.add_argument(
        "--size-hist",
        default="65536:10,262144:10,524288:18,786432:16,1048576:18,1572864:16,2097152:8,16777216:4",
        help="Comma-separated upper_bound_bytes:weight buckets",
    )
    p.add_argument("--put-prefix", required=True)
    p.add_argument("--output-csv", required=True)
    p.add_argument("--summary-json", required=True)
    p.add_argument("--metrics-before", default="")
    p.add_argument("--metrics-after", default="")
    p.add_argument("--metrics-url", default="http://127.0.0.1:9000/minio/metrics/v3")
    p.add_argument("--seed", type=int, default=1)
    p.add_argument("--run-id", default="")
    p.add_argument("--host-label", default="")
    p.add_argument("--report-interval", type=float, default=60.0, help="Seconds between live progress reports")
    p.add_argument("--metrics-sample-url", default="", help="Optional Buckit metrics URL to sample during live reports")
    p.add_argument("--delay-min-ms", type=float, default=0.0, help="Minimum random delay before each request")
    p.add_argument("--delay-max-ms", type=float, default=0.0, help="Maximum random delay before each request")
    return p.parse_args()


def parse_duration(raw: str) -> float:
    units = {"s": 1, "m": 60, "h": 3600}
    raw = raw.strip().lower()
    if raw[-1] not in units:
        raise ValueError(f"invalid duration: {raw}")
    return float(raw[:-1]) * units[raw[-1]]


def parse_hist(raw: str) -> list[tuple[int, float]]:
    parts = []
    for token in raw.split(","):
        upper, weight = token.split(":", 1)
        parts.append((int(upper), float(weight)))
    if not parts:
        raise ValueError("empty histogram")
    return parts


def read_keys(path: str) -> list[str]:
    with open(path, "r", encoding="utf-8") as fh:
        return [line.strip() for line in fh if line.strip()]


def build_client(cfg: ConfigData):
    return boto3.client(
        "s3",
        endpoint_url=cfg.endpoint,
        aws_access_key_id=cfg.access_key,
        aws_secret_access_key=cfg.secret_key,
        region_name=cfg.region,
        config=Config(signature_version="s3v4"),
    )


def weighted_choice(rng: random.Random, items: list[tuple[str, float]]) -> str:
    total = sum(weight for _, weight in items)
    pick = rng.random() * total
    upto = 0.0
    for value, weight in items:
        upto += weight
        if pick <= upto:
            return value
    return items[-1][0]


def size_from_hist(rng: random.Random, hist: list[tuple[int, float]]) -> int:
    bucket = weighted_choice(rng, [(str(upper), weight) for upper, weight in hist])
    upper = int(bucket)
    lower = 0
    for candidate, _ in hist:
        if candidate == upper:
            break
        lower = candidate
    if upper <= lower:
        return upper
    return rng.randint(lower + 1, upper)


def object_payload(key: str, size: int) -> bytes:
    seed = key.encode("utf-8")
    repeated = (seed * ((size // len(seed)) + 1))[:size]
    return repeated


def fetch_metrics(url: str) -> dict[str, float]:
    import urllib.request

    req = urllib.request.Request(url, method="GET")
    with urllib.request.urlopen(req, timeout=10) as resp:
        text = resp.read().decode("utf-8", errors="replace")

    totals: dict[str, float] = {}
    for line in text.splitlines():
        if not line or line.startswith("#"):
            continue
        metric_id, _, value_str = line.rpartition(" ")
        name = metric_id.split("{", 1)[0]
        try:
            value = float(value_str)
        except ValueError:
            continue
        totals[name] = totals.get(name, 0.0) + value
    return totals


def host_snapshot() -> dict[str, float]:
    try:
        load1, load5, load15 = os.getloadavg()
    except OSError:
        load1 = load5 = load15 = 0.0

    cpu_total = None
    mem_avail = None
    mem_total = None
    try:
        with open("/proc/meminfo", "r", encoding="utf-8") as fh:
            data = {}
            for line in fh:
                key, value = line.split(":", 1)
                data[key] = value.strip()
        mem_total = float(data["MemTotal"].split()[0]) * 1024.0
        mem_avail = float(data["MemAvailable"].split()[0]) * 1024.0
    except Exception:  # noqa: BLE001
        pass

    return {
        "load1": load1,
        "load5": load5,
        "load15": load15,
        "mem_total_bytes": mem_total if mem_total is not None else 0.0,
        "mem_available_bytes": mem_avail if mem_avail is not None else 0.0,
        "mem_used_bytes": (mem_total - mem_avail) if mem_total is not None and mem_avail is not None else 0.0,
    }


def percentile(values: list[float], pct: float) -> float | None:
    if not values:
        return None
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


def ensure_parent(path: str) -> None:
    Path(path).parent.mkdir(parents=True, exist_ok=True)


def do_get(client, cfg: ConfigData, key: str) -> Result:
    start = time.perf_counter()
    status = 0
    bytes_read = 0
    ttfb_ms = 0.0
    error = ""
    try:
        resp = client.get_object(Bucket=cfg.bucket, Key=key)
        status = resp["ResponseMetadata"]["HTTPStatusCode"]
        body = resp["Body"]
        while True:
            chunk = body.read(256 * 1024)
            if not chunk:
                break
            if bytes_read == 0:
                ttfb_ms = (time.perf_counter() - start) * 1000.0
            bytes_read += len(chunk)
        body.close()
    except Exception as exc:  # noqa: BLE001
        error = repr(exc)
    end = time.perf_counter()
    return Result("GET", key, status, bytes_read, bytes_read, ttfb_ms, (end - start) * 1000.0, error, start, end)


def do_put(client, cfg: ConfigData, key: str, payload: bytes) -> Result:
    start = time.perf_counter()
    status = 0
    error = ""
    try:
        resp = client.put_object(Bucket=cfg.bucket, Key=key, Body=payload, ContentType="application/octet-stream")
        status = resp["ResponseMetadata"]["HTTPStatusCode"]
    except Exception as exc:  # noqa: BLE001
        error = repr(exc)
    end = time.perf_counter()
    return Result("PUT", key, status, len(payload), len(payload), 0.0, (end - start) * 1000.0, error, start, end)


def do_delete(client, cfg: ConfigData, key: str, object_bytes: int) -> Result:
    start = time.perf_counter()
    status = 0
    error = ""
    try:
        resp = client.delete_object(Bucket=cfg.bucket, Key=key)
        status = resp["ResponseMetadata"]["HTTPStatusCode"]
    except Exception as exc:  # noqa: BLE001
        error = repr(exc)
    end = time.perf_counter()
    return Result("DELETE", key, status, 0, object_bytes, 0.0, (end - start) * 1000.0, error, start, end)


def choose_op(rng: random.Random, ops: list[tuple[str, float]], pool: KeyPool, byte_balance: ByteBalance) -> str:
    put_weight = next((weight for op, weight in ops if op == "PUT"), 0.0)
    delete_weight = next((weight for op, weight in ops if op == "DELETE"), 0.0)
    get_weight = next((weight for op, weight in ops if op == "GET"), 0.0)
    if put_weight > 0 and delete_weight > 0:
        put_bytes, delete_bytes = byte_balance.snapshot()
        if put_bytes > delete_bytes and pool.snapshot_mutable_size() > 0:
            return weighted_choice(rng, [("GET", get_weight), ("DELETE", put_weight + delete_weight)])
        if delete_bytes > put_bytes:
            return weighted_choice(rng, [("GET", get_weight), ("PUT", put_weight + delete_weight)])
    return weighted_choice(rng, ops)


def worker_submit(
    client,
    cfg: ConfigData,
    pool: KeyPool,
    rng: random.Random,
    hist: list[tuple[int, float]],
    ops: list[tuple[str, float]],
    byte_balance: ByteBalance,
):
    op = choose_op(rng, ops, pool, byte_balance)
    if op == "GET":
        key = pool.random_current(rng)
        if key is None:
            return None
        return do_get(client, cfg, key)
    if op == "PUT":
        key = pool.new_put_key()
        size = size_from_hist(rng, hist)
        payload = object_payload(key, size)
        result = do_put(client, cfg, key, payload)
        if not result.error and 200 <= result.status < 300:
            pool.commit_put(key, size)
        return result
    if op == "DELETE":
        put_bytes, delete_bytes = byte_balance.snapshot()
        reservation = pool.reserve_delete(rng, target_bytes=max(put_bytes - delete_bytes, 0))
        if reservation is None:
            return None
        key, size = reservation
        result = do_delete(client, cfg, key, size)
        if result.error or not (200 <= result.status < 300):
            pool.restore(key, size)
        return result
    raise ValueError(f"unsupported op {op}")


def main() -> int:
    args = parse_args()
    cfg = ConfigData(
        endpoint=args.endpoint,
        bucket=args.bucket,
        access_key=args.access_key,
        secret_key=args.secret_key,
        region=args.region,
        concurrency=args.concurrency,
        duration=parse_duration(args.duration),
        get_weight=args.get_weight,
        put_weight=args.put_weight,
        delete_weight=args.delete_weight,
        size_hist=args.size_hist,
        put_prefix=args.put_prefix,
        output_csv=args.output_csv,
        summary_json=args.summary_json,
        metrics_before=args.metrics_before,
        metrics_after=args.metrics_after,
        metrics_url=args.metrics_url,
        seed=args.seed,
        run_id=args.run_id or dt.datetime.now(UTC).strftime("%Y%m%dT%H%M%SZ"),
        host_label=args.host_label or args.endpoint,
        report_interval=args.report_interval,
        metrics_sample_url=args.metrics_sample_url,
        delay_min_ms=args.delay_min_ms,
        delay_max_ms=args.delay_max_ms,
    )

    if cfg.delay_min_ms < 0 or cfg.delay_max_ms < 0:
        raise ValueError("delay values must be non-negative")
    if cfg.delay_max_ms < cfg.delay_min_ms:
        raise ValueError("delay-max-ms must be >= delay-min-ms")

    keys = read_keys(args.keys_file)
    pool = KeyPool(keys, cfg.put_prefix)
    hist = parse_hist(cfg.size_hist)
    ops = [("GET", cfg.get_weight), ("PUT", cfg.put_weight), ("DELETE", cfg.delete_weight)]

    metrics_before = fetch_metrics(cfg.metrics_url) if cfg.metrics_before else {}
    live_metrics_prev = fetch_metrics(cfg.metrics_sample_url) if cfg.metrics_sample_url else {}
    if cfg.metrics_before:
        ensure_parent(cfg.metrics_before)
        Path(cfg.metrics_before).write_text(json.dumps(metrics_before, indent=2, sort_keys=True), encoding="utf-8")

    client = build_client(cfg)
    deadline = time.monotonic() + cfg.duration
    results: list[Result] = []
    counts = Counter()
    lock = threading.Lock()
    byte_balance = ByteBalance()
    last_report_at = time.monotonic()
    last_report_index = 0
    started_at = time.monotonic()

    def task(idx: int):
        rng = random.Random(cfg.seed + idx + int(time.time_ns()))
        if cfg.delay_max_ms > 0:
            time.sleep(rng.uniform(cfg.delay_min_ms, cfg.delay_max_ms) / 1000.0)
        return worker_submit(client, cfg, pool, rng, hist, ops, byte_balance)

    submitted = 0
    with concurrent.futures.ThreadPoolExecutor(max_workers=cfg.concurrency) as executor:
        futures = set()
        while time.monotonic() < deadline and len(futures) < cfg.concurrency:
            futures.add(executor.submit(task, submitted))
            submitted += 1
        while futures:
            done, futures = concurrent.futures.wait(futures, return_when=concurrent.futures.FIRST_COMPLETED)
            for fut in done:
                result = fut.result()
                if result is not None:
                    with lock:
                        results.append(result)
                        counts[result.op] += 1
                        if not result.error and 200 <= result.status < 300:
                            byte_balance.note(result.op, result.object_bytes)
                        now = time.monotonic()
                        should_report = cfg.report_interval > 0 and now - last_report_at >= cfg.report_interval
                        if should_report:
                            window = results[last_report_index:]
                            last_report_index = len(results)
                            last_report_at = now
                            window_put_bytes, window_delete_bytes = byte_balance.reset()
                            window_counts = Counter(r.op for r in window)
                            window_errors = Counter(r.op for r in window if r.error or not (200 <= r.status < 300))
                            window_elapsed = max((window[-1].end_ts - window[0].start_ts), 1e-9) if window else 1e-9
                            total_elapsed = max((results[-1].end_ts - results[0].start_ts), 1e-9) if results else 1e-9
                            bytes_window = sum(r.bytes_read for r in window)
                            bytes_total = sum(r.bytes_read for r in results)
                            host = host_snapshot()
                            ts = dt.datetime.now(UTC).isoformat(timespec="seconds")
                            elapsed_wall = now - started_at
                            live_metrics_now = fetch_metrics(cfg.metrics_sample_url) if cfg.metrics_sample_url else {}
                            live_metric_delta = {}
                            for name in (
                                "minio_api_requests_fast_open_hits_total",
                                "minio_api_requests_fast_open_attempted_total",
                                "minio_api_requests_fast_get_hits_total",
                                "minio_api_requests_fast_get_fallbacks_total",
                                "minio_api_requests_fast_open_final_errors_total",
                                "minio_api_requests_fast_open_stream_cancellations_total",
                            ):
                                if live_metrics_now:
                                    live_metric_delta[name] = live_metrics_now.get(name, 0.0) - live_metrics_prev.get(name, 0.0)
                            live_metrics_prev = live_metrics_now or live_metrics_prev
                            parts = [
                                f"ts={ts}",
                                f"elapsed_s={elapsed_wall:.1f}",
                                f"window_s={window_elapsed:.1f}",
                                f"progress={len(results)}",
                                f"pool={pool.snapshot_size()}",
                                f"read_pool={pool.snapshot_read_size()}",
                                f"mutable_pool={pool.snapshot_mutable_size()}",
                                f"counts={dict(counts)}",
                                f"window_counts={dict(window_counts)}",
                                f"window_errors={dict(window_errors)}",
                                f"window_objps={len(window)/window_elapsed:.2f}",
                                f"total_objps={len(results)/total_elapsed:.2f}",
                                f"window_mibps={bytes_window / window_elapsed / (1024*1024):.2f}",
                                f"total_mibps={bytes_total / total_elapsed / (1024*1024):.2f}",
                                f"window_put_mib={window_put_bytes / (1024*1024):.2f}",
                                f"window_delete_mib={window_delete_bytes / (1024*1024):.2f}",
                                f"window_put_delete_delta_mib={(window_put_bytes - window_delete_bytes) / (1024*1024):.2f}",
                                f"host_load1={host['load1']:.2f}",
                                f"host_load5={host['load5']:.2f}",
                            ]
                            if host["mem_total_bytes"] > 0:
                                mem_used_pct = host["mem_used_bytes"] / host["mem_total_bytes"] * 100.0
                                parts.append(f"host_mem_used_pct={mem_used_pct:.1f}")
                            for op_name in ("GET", "PUT", "DELETE"):
                                op_results = [r for r in window if r.op == op_name and not r.error and 200 <= r.status < 300]
                                if not op_results:
                                    continue
                                total_stats = summarize([r.total_ms for r in op_results])
                                parts.append(
                                    f"{op_name.lower()}_total_ms(mean={total_stats['mean']:.1f},p50={total_stats['p50']:.1f},p90={total_stats['p90']:.1f},p99={total_stats['p99']:.1f})"
                                )
                                if op_name == "GET":
                                    ttfb_stats = summarize([r.ttfb_ms for r in op_results if r.ttfb_ms > 0])
                                    if ttfb_stats["count"]:
                                        parts.append(
                                            f"get_ttfb_ms(mean={ttfb_stats['mean']:.1f},p50={ttfb_stats['p50']:.1f},p90={ttfb_stats['p90']:.1f},p99={ttfb_stats['p99']:.1f})"
                                        )
                            if live_metric_delta:
                                parts.append(
                                    "fastopen_delta="
                                    + json.dumps({k.rsplit("_", 1)[0]: v for k, v in live_metric_delta.items()}, sort_keys=True)
                                )
                            print(" ".join(parts), flush=True)
                if time.monotonic() < deadline:
                    futures.add(executor.submit(task, submitted))
                    submitted += 1

    metrics_after = fetch_metrics(cfg.metrics_url) if cfg.metrics_after else {}
    if cfg.metrics_after:
        ensure_parent(cfg.metrics_after)
        Path(cfg.metrics_after).write_text(json.dumps(metrics_after, indent=2, sort_keys=True), encoding="utf-8")

    ensure_parent(cfg.output_csv)
    with open(cfg.output_csv, "w", newline="", encoding="utf-8") as fh:
        writer = csv.writer(fh)
        writer.writerow(["run_id", "host_label", "op", "key", "status", "bytes", "object_bytes", "ttfb_ms", "total_ms", "error", "start_ts", "end_ts"])
        for result in results:
            writer.writerow([
                cfg.run_id,
                cfg.host_label,
                result.op,
                result.key,
                result.status,
                result.bytes_read,
                result.object_bytes,
                f"{result.ttfb_ms:.3f}",
                f"{result.total_ms:.3f}",
                result.error,
                f"{result.start_ts:.6f}",
                f"{result.end_ts:.6f}",
            ])

    per_op = {}
    for op in ("GET", "PUT", "DELETE"):
        op_results = [r for r in results if r.op == op and not r.error and 200 <= r.status < 300]
        per_op[op] = {
            "count": len(op_results),
            "errors": len([r for r in results if r.op == op]) - len(op_results),
            "bytes": sum(r.bytes_read for r in op_results),
            "ttfb_ms": summarize([r.ttfb_ms for r in op_results if r.ttfb_ms > 0]),
            "total_ms": summarize([r.total_ms for r in op_results]),
        }

    summary = {
        "run_id": cfg.run_id,
        "host_label": cfg.host_label,
        "bucket": cfg.bucket,
        "endpoint": cfg.endpoint,
        "duration_seconds": cfg.duration,
        "concurrency": cfg.concurrency,
        "initial_key_count": len(keys),
        "initial_read_key_count": pool.snapshot_read_size(),
        "final_key_pool_size": pool.snapshot_size(),
        "final_mutable_key_count": pool.snapshot_mutable_size(),
        "submitted": submitted,
        "completed": len(results),
        "counts": dict(counts),
        "per_op": per_op,
        "metrics_before": metrics_before,
        "metrics_after": metrics_after,
    }
    ensure_parent(cfg.summary_json)
    Path(cfg.summary_json).write_text(json.dumps(summary, indent=2, sort_keys=True), encoding="utf-8")
    print(f"output_csv={cfg.output_csv}")
    print(f"summary_json={cfg.summary_json}")
    print(f"completed={len(results)} counts={dict(counts)} final_key_pool_size={pool.snapshot_size()}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
