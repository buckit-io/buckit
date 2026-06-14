#!/usr/bin/env python3
"""Seed deterministic objects and run controlled local FastOpen GET plans."""

from __future__ import annotations

import argparse
import concurrent.futures
import csv
import datetime as dt
import hashlib
import hmac
import json
import math
import random
import re
import statistics
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from pathlib import Path


UNSIGNED_PAYLOAD = "UNSIGNED-PAYLOAD"
UTC = getattr(dt, "UTC", dt.timezone.utc)
DEFAULT_METRIC_NAMES = [
    "minio_api_requests_fast_get_hits_total",
    "minio_api_requests_fast_get_fallbacks_total",
    "minio_api_requests_fast_open_attempted_total",
    "minio_api_requests_fast_open_hits_total",
    "minio_api_requests_fast_open_unsupported_total",
    "minio_api_requests_fast_open_replacement_path_total",
    "minio_api_requests_fast_open_streams_opened_total",
    "minio_api_requests_fast_open_replacement_opens_total",
    "minio_api_requests_fast_open_selected_set_failures_total",
    "minio_api_requests_fast_open_stream_cancellations_total",
    "minio_api_requests_fast_open_final_errors_total",
    "minio_api_requests_fast_open_httptrace_connections_total",
    "minio_api_requests_fast_open_httptrace_reused_connections_total",
    "minio_api_requests_fast_open_httptrace_fresh_connections_total",
    "minio_api_requests_fast_open_httptrace_was_idle_connections_total",
]


@dataclass(frozen=True)
class CommonConfig:
    scheme: str
    host: str
    bucket: str
    access_key: str
    secret_key: str
    region: str
    timeout: float


@dataclass(frozen=True)
class SeedConfig:
    common: CommonConfig
    object_size: int
    object_count: int
    key_prefix: str
    start_index: int
    keys_output: str
    overwrite: bool


@dataclass(frozen=True)
class RunConfig:
    common: CommonConfig
    keys_file: str
    concurrency: int
    cache_profile: str
    ordering: str
    hot_fraction: float
    seed: int
    path_arm: str
    run_id: str
    object_size_label: str
    output_csv: str
    summary_json: str
    plan_output: str
    metrics_url: str
    metrics_before: str
    metrics_after: str


@dataclass(frozen=True)
class PlannedRequest:
    request_index: int
    key: str
    access_number: int


@dataclass
class Result:
    request_index: int
    key: str
    access_number: int
    status: int
    bytes_read: int
    ttfb_ms: float
    total_ms: float
    error: str


def sign(key: bytes, msg: str) -> bytes:
    return hmac.new(key, msg.encode("utf-8"), hashlib.sha256).digest()


def signing_key(secret_key: str, date: str, region: str) -> bytes:
    k_date = sign(("AWS4" + secret_key).encode("utf-8"), date)
    k_region = sign(k_date, region)
    k_service = sign(k_region, "s3")
    return sign(k_service, "aws4_request")


def quote_path(path: str) -> str:
    return urllib.parse.quote(path, safe="/-_.~")


def canonical_query(params: dict[str, str]) -> str:
    parts = []
    for key in sorted(params):
        parts.append(
            urllib.parse.quote(key, safe="-_.~")
            + "="
            + urllib.parse.quote(params[key], safe="-_.~")
        )
    return "&".join(parts)


def signed_request(
    cfg: CommonConfig,
    method: str,
    path: str,
    *,
    params: dict[str, str] | None = None,
    payload: bytes | None = None,
    extra_headers: dict[str, str] | None = None,
) -> urllib.request.Request:
    params = params or {}
    extra_headers = extra_headers or {}
    now = dt.datetime.now(UTC)
    amz_date = now.strftime("%Y%m%dT%H%M%SZ")
    date_stamp = now.strftime("%Y%m%d")
    canonical_uri = quote_path(path)
    canonical_qs = canonical_query(params)
    payload_hash = (
        hashlib.sha256(payload).hexdigest() if payload is not None else UNSIGNED_PAYLOAD
    )

    headers = {
        "host": cfg.host,
        "x-amz-content-sha256": payload_hash,
        "x-amz-date": amz_date,
    }
    for key, value in extra_headers.items():
        headers[key.lower()] = value

    signed_headers = ";".join(sorted(headers))
    canonical_headers = "".join(f"{key}:{headers[key]}\n" for key in sorted(headers))
    canonical_req = "\n".join(
        [
            method,
            canonical_uri,
            canonical_qs,
            canonical_headers,
            signed_headers,
            payload_hash,
        ]
    )
    scope = f"{date_stamp}/{cfg.region}/s3/aws4_request"
    string_to_sign = "\n".join(
        [
            "AWS4-HMAC-SHA256",
            amz_date,
            scope,
            hashlib.sha256(canonical_req.encode("utf-8")).hexdigest(),
        ]
    )
    signature = hmac.new(
        signing_key(cfg.secret_key, date_stamp, cfg.region),
        string_to_sign.encode("utf-8"),
        hashlib.sha256,
    ).hexdigest()
    auth = (
        "AWS4-HMAC-SHA256 "
        f"Credential={cfg.access_key}/{scope}, "
        f"SignedHeaders={signed_headers}, "
        f"Signature={signature}"
    )

    final_headers = {
        "Host": cfg.host,
        "Authorization": auth,
        "x-amz-content-sha256": payload_hash,
        "x-amz-date": amz_date,
    }
    final_headers.update(extra_headers)

    url = f"{cfg.scheme}://{cfg.host}{canonical_uri}"
    if canonical_qs:
        url += "?" + canonical_qs
    return urllib.request.Request(
        url,
        method=method,
        headers=final_headers,
        data=payload,
    )


def parse_size(raw: str) -> int:
    match = re.fullmatch(r"(?i)\s*(\d+)\s*([kmgt]?i?b)?\s*", raw)
    if not match:
        raise ValueError(f"invalid size: {raw}")
    count = int(match.group(1))
    unit = (match.group(2) or "b").lower()
    multipliers = {
        "b": 1,
        "kib": 1024,
        "kb": 1000,
        "mib": 1024**2,
        "mb": 1000**2,
        "gib": 1024**3,
        "gb": 1000**3,
        "tib": 1024**4,
        "tb": 1000**4,
    }
    return count * multipliers[unit]


def ensure_parent(path: str) -> None:
    if path:
        Path(path).parent.mkdir(parents=True, exist_ok=True)


def ensure_bucket(cfg: CommonConfig) -> None:
    req = signed_request(cfg, "PUT", f"/{cfg.bucket}")
    try:
        with urllib.request.urlopen(req, timeout=cfg.timeout):
            return
    except urllib.error.HTTPError as exc:
        if exc.code in (200, 204, 409):
            return
        body = exc.read().decode("utf-8", errors="replace")
        raise RuntimeError(f"create bucket failed: {exc.code} {body}") from exc


def object_payload(key: str, size: int) -> bytes:
    seed = hashlib.sha256(key.encode("utf-8")).digest()
    repeated = (seed * ((size // len(seed)) + 1))[:size]
    return repeated


def seed_objects(cfg: SeedConfig) -> int:
    ensure_bucket(cfg.common)
    keys: list[str] = []
    for offset in range(cfg.object_count):
        index = cfg.start_index + offset
        key = f"{cfg.key_prefix}{index:06d}"
        payload = object_payload(key, cfg.object_size)
        headers = {
            "Content-Length": str(len(payload)),
            "Content-Type": "application/octet-stream",
        }
        req = signed_request(
            cfg.common,
            "PUT",
            f"/{cfg.common.bucket}/{key}",
            payload=payload,
            extra_headers=headers,
        )
        try:
            with urllib.request.urlopen(req, timeout=cfg.common.timeout) as resp:
                if resp.getcode() not in (200, 201):
                    raise RuntimeError(f"PUT {key}: unexpected HTTP {resp.getcode()}")
        except urllib.error.HTTPError as exc:
            body = exc.read().decode("utf-8", errors="replace")
            if exc.code == 409 and not cfg.overwrite:
                pass
            else:
                raise RuntimeError(f"PUT {key} failed: {exc.code} {body}") from exc
        keys.append(key)
        if (offset + 1) % 25 == 0 or offset + 1 == cfg.object_count:
            print(f"seeded={offset + 1}/{cfg.object_count}", flush=True)

    if cfg.keys_output:
        ensure_parent(cfg.keys_output)
        with open(cfg.keys_output, "w", encoding="utf-8") as fh:
            for key in keys:
                fh.write(key + "\n")
        print(f"keys_file={cfg.keys_output}")
    return 0


def read_keys(path: str) -> list[str]:
    with open(path, "r", encoding="utf-8") as fh:
        return [line.strip() for line in fh if line.strip()]


def hot_set(keys: list[str], fraction: float) -> list[str]:
    count = max(1, math.ceil(len(keys) * fraction))
    return keys[:count]


def profile_counts(keys: list[str], profile: str, hot_fraction: float) -> dict[str, int]:
    counts = {key: 1 for key in keys}
    if profile == "1x-all":
        return counts
    if profile == "2x-all":
        return {key: 2 for key in keys}
    if profile in {"10pct-hot", "20pct-hot", "50pct-hot"}:
        pct = {
            "10pct-hot": 0.10,
            "20pct-hot": 0.20,
            "50pct-hot": 0.50,
        }[profile]
        for key in hot_set(keys, pct):
            counts[key] = 2
        return counts
    if profile == "hotset-5x":
        for key in hot_set(keys, hot_fraction):
            counts[key] = 5
        return counts
    if profile == "hotset-10x":
        for key in hot_set(keys, hot_fraction):
            counts[key] = 10
        return counts
    raise ValueError(f"unsupported cache profile: {profile}")


def build_plan(keys: list[str], profile: str, ordering: str, seed: int, hot_fraction: float) -> list[PlannedRequest]:
    counts = profile_counts(keys, profile, hot_fraction)
    requests: list[str] = []

    if ordering == "key-order":
        requests.extend(keys)
    elif ordering == "immediate-repeat":
        for key in keys:
            requests.extend([key] * counts[key])
    elif ordering == "pass-repeat":
        requests.extend(keys)
        max_repeat = max(counts.values())
        for access_number in range(2, max_repeat + 1):
            for key in keys:
                if counts[key] >= access_number:
                    requests.append(key)
    elif ordering == "shuffled":
        for key in keys:
            requests.extend([key] * counts[key])
        random.Random(seed).shuffle(requests)
    else:
        raise ValueError(f"unsupported ordering: {ordering}")

    seen: dict[str, int] = {}
    plan: list[PlannedRequest] = []
    for index, key in enumerate(requests):
        seen[key] = seen.get(key, 0) + 1
        plan.append(PlannedRequest(index, key, seen[key]))
    return plan


def fetch_metrics(url: str, timeout: float) -> dict[str, float]:
    req = urllib.request.Request(url, method="GET")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            text = resp.read().decode("utf-8", errors="replace")
    except Exception as exc:  # noqa: BLE001
        raise RuntimeError(f"metrics scrape failed: {exc!r}") from exc

    totals: dict[str, float] = {}
    for line in text.splitlines():
        if not line or line.startswith("#"):
            continue
        metric_id, _, value_str = line.rpartition(" ")
        name = metric_id.split("{", 1)[0]
        if name not in DEFAULT_METRIC_NAMES:
            continue
        try:
            value = float(value_str)
        except ValueError:
            continue
        totals[name] = totals.get(name, 0.0) + value
    return totals


def fetch_one(cfg: CommonConfig, req: PlannedRequest) -> Result:
    start = time.perf_counter()
    status = 0
    bytes_read = 0
    ttfb_ms = 0.0
    err = ""
    try:
        http_req = signed_request(cfg, "GET", f"/{cfg.bucket}/{req.key}")
        with urllib.request.urlopen(http_req, timeout=cfg.timeout) as resp:
            status = resp.getcode()
            ttfb_ms = (time.perf_counter() - start) * 1000.0
            while True:
                chunk = resp.read(256 * 1024)
                if not chunk:
                    break
                bytes_read += len(chunk)
    except urllib.error.HTTPError as exc:
        status = exc.code
        err = str(exc)
        try:
            exc.read()
        except Exception:
            pass
    except Exception as exc:  # noqa: BLE001
        err = repr(exc)
    total_ms = (time.perf_counter() - start) * 1000.0
    return Result(
        request_index=req.request_index,
        key=req.key,
        access_number=req.access_number,
        status=status,
        bytes_read=bytes_read,
        ttfb_ms=ttfb_ms,
        total_ms=total_ms,
        error=err,
    )


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


def summary_for_results(results: list[Result]) -> dict[str, object]:
    ok = [r for r in results if 200 <= r.status < 300 and not r.error]
    first = [r for r in ok if r.access_number == 1]
    repeated = [r for r in ok if r.access_number > 1]
    return {
        "all_requests": {
            "ttfb_ms": summarize([r.ttfb_ms for r in ok]),
            "total_ms": summarize([r.total_ms for r in ok]),
            "bytes_read": sum(r.bytes_read for r in ok),
            "errors": len(results) - len(ok),
        },
        "first_access": {
            "ttfb_ms": summarize([r.ttfb_ms for r in first]),
            "total_ms": summarize([r.total_ms for r in first]),
            "bytes_read": sum(r.bytes_read for r in first),
            "errors": len([r for r in results if r.access_number == 1]) - len(first),
        },
        "repeated_access": {
            "ttfb_ms": summarize([r.ttfb_ms for r in repeated]),
            "total_ms": summarize([r.total_ms for r in repeated]),
            "bytes_read": sum(r.bytes_read for r in repeated),
            "errors": len([r for r in results if r.access_number > 1]) - len(repeated),
        },
    }


def write_plan(path: str, plan: list[PlannedRequest]) -> None:
    if not path:
        return
    ensure_parent(path)
    with open(path, "w", newline="", encoding="utf-8") as fh:
        writer = csv.writer(fh)
        writer.writerow(["request_index", "key", "access_number"])
        for item in plan:
            writer.writerow([item.request_index, item.key, item.access_number])


def write_results(path: str, cfg: RunConfig, results: list[Result]) -> None:
    ensure_parent(path)
    with open(path, "w", newline="", encoding="utf-8") as fh:
        writer = csv.writer(fh)
        writer.writerow(
            [
                "run_id",
                "path_arm",
                "object_size",
                "cache_profile",
                "ordering",
                "concurrency",
                "request_index",
                "key",
                "access_number",
                "access_class",
                "ttfb_ms",
                "total_ms",
                "bytes",
                "status",
                "error",
            ]
        )
        for r in sorted(results, key=lambda item: item.request_index):
            writer.writerow(
                [
                    cfg.run_id,
                    cfg.path_arm,
                    cfg.object_size_label,
                    cfg.cache_profile,
                    cfg.ordering,
                    cfg.concurrency,
                    r.request_index,
                    r.key,
                    r.access_number,
                    "first" if r.access_number == 1 else "repeated",
                    f"{r.ttfb_ms:.3f}",
                    f"{r.total_ms:.3f}",
                    r.bytes_read,
                    r.status,
                    r.error,
                ]
            )


def run_plan(cfg: RunConfig) -> int:
    keys = read_keys(cfg.keys_file)
    if not keys:
        print("no keys loaded", file=sys.stderr)
        return 2
    plan = build_plan(keys, cfg.cache_profile, cfg.ordering, cfg.seed, cfg.hot_fraction)
    if cfg.plan_output:
        write_plan(cfg.plan_output, plan)

    metrics_before = fetch_metrics(cfg.metrics_url, cfg.common.timeout) if cfg.metrics_url else {}
    if cfg.metrics_before:
        ensure_parent(cfg.metrics_before)
        with open(cfg.metrics_before, "w", encoding="utf-8") as fh:
            json.dump(metrics_before, fh, indent=2, sort_keys=True)

    print(
        f"run_id={cfg.run_id} arm={cfg.path_arm} keys={len(keys)} requests={len(plan)} "
        f"profile={cfg.cache_profile} ordering={cfg.ordering} concurrency={cfg.concurrency}",
        flush=True,
    )

    results: list[Result] = []
    lock = threading.Lock()
    started = time.perf_counter()
    completed = 0
    with concurrent.futures.ThreadPoolExecutor(max_workers=cfg.concurrency) as pool:
        futures = [pool.submit(fetch_one, cfg.common, req) for req in plan]
        for future in concurrent.futures.as_completed(futures):
            result = future.result()
            with lock:
                results.append(result)
                completed += 1
                if completed % 100 == 0 or completed == len(plan):
                    print(f"progress={completed}/{len(plan)}", flush=True)

    elapsed = time.perf_counter() - started
    metrics_after = fetch_metrics(cfg.metrics_url, cfg.common.timeout) if cfg.metrics_url else {}
    if cfg.metrics_after:
        ensure_parent(cfg.metrics_after)
        with open(cfg.metrics_after, "w", encoding="utf-8") as fh:
            json.dump(metrics_after, fh, indent=2, sort_keys=True)

    metric_delta = {
        name: metrics_after.get(name, 0.0) - metrics_before.get(name, 0.0)
        for name in DEFAULT_METRIC_NAMES
        if metrics_before or metrics_after
    }
    write_results(cfg.output_csv, cfg, results)

    summary = {
        "run_id": cfg.run_id,
        "path_arm": cfg.path_arm,
        "object_size": cfg.object_size_label,
        "cache_profile": cfg.cache_profile,
        "ordering": cfg.ordering,
        "concurrency": cfg.concurrency,
        "key_count": len(keys),
        "request_count": len(plan),
        "elapsed_seconds": elapsed,
        "metrics_before": metrics_before,
        "metrics_after": metrics_after,
        "metrics_delta": metric_delta,
        "results": summary_for_results(results),
    }
    ensure_parent(cfg.summary_json)
    with open(cfg.summary_json, "w", encoding="utf-8") as fh:
        json.dump(summary, fh, indent=2, sort_keys=True)

    print(f"output_csv={cfg.output_csv}")
    print(f"summary_json={cfg.summary_json}")
    if metric_delta:
        print("metrics_delta=" + json.dumps(metric_delta, sort_keys=True))
    return 0 if summary["results"]["all_requests"]["errors"] == 0 else 1


def parse_common(args: argparse.Namespace) -> CommonConfig:
    return CommonConfig(
        scheme=args.scheme,
        host=args.host,
        bucket=args.bucket,
        access_key=args.access_key,
        secret_key=args.secret_key,
        region=args.region,
        timeout=args.timeout,
    )


def add_common_flags(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--host", default="127.0.0.1:9000")
    parser.add_argument("--bucket", default="fastopen-bench")
    parser.add_argument("--access-key", default="buckitadmin")
    parser.add_argument("--secret-key", default="buckitadmin")
    parser.add_argument("--region", default="us-east-1")
    parser.add_argument("--scheme", default="http", choices=["http", "https"])
    parser.add_argument("--timeout", type=float, default=30.0)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)

    seed = sub.add_parser("seed", help="Create bucket and deterministic objects")
    add_common_flags(seed)
    seed.add_argument("--object-size", required=True, help="Examples: 640KiB, 2MiB")
    seed.add_argument("--object-count", type=int, required=True)
    seed.add_argument("--key-prefix", default="obj-")
    seed.add_argument("--start-index", type=int, default=1)
    seed.add_argument("--keys-output", default="")
    seed.add_argument("--overwrite", action="store_true")

    run = sub.add_parser("run", help="Run a controlled GET plan")
    add_common_flags(run)
    run.add_argument("--keys-file", required=True)
    run.add_argument("--concurrency", type=int, default=1)
    run.add_argument(
        "--cache-profile",
        required=True,
        choices=[
            "1x-all",
            "2x-all",
            "10pct-hot",
            "20pct-hot",
            "50pct-hot",
            "hotset-5x",
            "hotset-10x",
        ],
    )
    run.add_argument(
        "--ordering",
        default="pass-repeat",
        choices=["key-order", "immediate-repeat", "pass-repeat", "shuffled"],
    )
    run.add_argument("--hot-fraction", type=float, default=0.10)
    run.add_argument("--seed", type=int, default=1)
    run.add_argument("--path-arm", required=True, choices=["OFF", "ON"])
    run.add_argument("--run-id", default="")
    run.add_argument("--object-size-label", default="")
    run.add_argument("--output-csv", required=True)
    run.add_argument("--summary-json", required=True)
    run.add_argument("--plan-output", default="")
    run.add_argument(
        "--metrics-url",
        default="http://127.0.0.1:9000/minio/metrics/v3/api/requests",
    )
    run.add_argument("--metrics-before", default="")
    run.add_argument("--metrics-after", default="")

    return parser.parse_args()


def main() -> int:
    args = parse_args()
    if args.command == "seed":
        cfg = SeedConfig(
            common=parse_common(args),
            object_size=parse_size(args.object_size),
            object_count=args.object_count,
            key_prefix=args.key_prefix,
            start_index=args.start_index,
            keys_output=args.keys_output,
            overwrite=args.overwrite,
        )
        return seed_objects(cfg)

    run_id = args.run_id or dt.datetime.now(UTC).strftime("%Y%m%dT%H%M%SZ")
    object_size_label = args.object_size_label or "unspecified"
    cfg = RunConfig(
        common=parse_common(args),
        keys_file=args.keys_file,
        concurrency=args.concurrency,
        cache_profile=args.cache_profile,
        ordering=args.ordering,
        hot_fraction=args.hot_fraction,
        seed=args.seed,
        path_arm=args.path_arm,
        run_id=run_id,
        object_size_label=object_size_label,
        output_csv=args.output_csv,
        summary_json=args.summary_json,
        plan_output=args.plan_output,
        metrics_url=args.metrics_url,
        metrics_before=args.metrics_before,
        metrics_after=args.metrics_after,
    )
    return run_plan(cfg)


if __name__ == "__main__":
    raise SystemExit(main())
