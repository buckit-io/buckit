#!/usr/bin/env python3
"""Issue S3 GETs over a listed object set.

This is intended for cache-sensitive HDD GET tests where duration-based tools can
repeat hot objects. The script lists keys once, optionally shuffles them, then
dispatches each requested GET with bounded concurrency. Failed requests are
recorded but not retried by default.
"""

from __future__ import annotations

import argparse
import concurrent.futures
import csv
import datetime as dt
import hashlib
import hmac
import random
import statistics
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import xml.etree.ElementTree as ET
from dataclasses import dataclass


UNSIGNED_PAYLOAD = "UNSIGNED-PAYLOAD"


@dataclass(frozen=True)
class Config:
    scheme: str
    hosts: list[str]
    bucket: str
    access_key: str
    secret_key: str
    region: str
    prefix: str
    suffix: str
    concurrency: int
    limit: int
    timeout: float
    shuffle: bool
    seed: int
    host_select: str
    output: str
    keys_file: str
    write_keys: str
    repeat: int
    repeat_mode: str
    progress_every: int


@dataclass
class Result:
    seq: int
    key: str
    host: str
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
    cfg: Config,
    method: str,
    host: str,
    path: str,
    params: dict[str, str] | None = None,
) -> urllib.request.Request:
    params = params or {}
    now = dt.datetime.now(dt.UTC)
    amz_date = now.strftime("%Y%m%dT%H%M%SZ")
    date_stamp = now.strftime("%Y%m%d")
    canonical_uri = quote_path(path)
    canonical_qs = canonical_query(params)
    signed_headers = "host;x-amz-content-sha256;x-amz-date"
    canonical_headers = (
        f"host:{host}\n"
        f"x-amz-content-sha256:{UNSIGNED_PAYLOAD}\n"
        f"x-amz-date:{amz_date}\n"
    )
    canonical_req = "\n".join(
        [
            method,
            canonical_uri,
            canonical_qs,
            canonical_headers,
            signed_headers,
            UNSIGNED_PAYLOAD,
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
    url = f"{cfg.scheme}://{host}{canonical_uri}"
    if canonical_qs:
        url += "?" + canonical_qs
    return urllib.request.Request(
        url,
        method=method,
        headers={
            "Host": host,
            "Authorization": auth,
            "x-amz-content-sha256": UNSIGNED_PAYLOAD,
            "x-amz-date": amz_date,
        },
    )


def parse_list_objects(data: bytes) -> tuple[list[str], bool, str]:
    root = ET.fromstring(data)
    ns = ""
    if root.tag.startswith("{"):
        ns = root.tag.split("}", 1)[0] + "}"
    keys = [elem.text or "" for elem in root.findall(f".//{ns}Contents/{ns}Key")]
    truncated = (root.findtext(f"{ns}IsTruncated") or "").lower() == "true"
    token = root.findtext(f"{ns}NextContinuationToken") or ""
    return keys, truncated, token


def list_keys(cfg: Config) -> list[str]:
    if cfg.keys_file:
        with open(cfg.keys_file, "r") as fh:
            keys = [line.rstrip("\n") for line in fh if line.rstrip("\n")]
        if cfg.prefix:
            keys = [key for key in keys if key.startswith(cfg.prefix)]
        if cfg.suffix:
            keys = [key for key in keys if key.endswith(cfg.suffix)]
        if cfg.limit:
            keys = keys[: cfg.limit]
        return keys

    host = cfg.hosts[0]
    keys: list[str] = []
    token = ""
    while True:
        params = {
            "list-type": "2",
            "max-keys": "1000",
        }
        if cfg.prefix:
            params["prefix"] = cfg.prefix
        if token:
            params["continuation-token"] = token
        req = signed_request(cfg, "GET", host, f"/{cfg.bucket}", params)
        with urllib.request.urlopen(req, timeout=cfg.timeout) as resp:
            batch, truncated, token = parse_list_objects(resp.read())
        for key in batch:
            if cfg.suffix and not key.endswith(cfg.suffix):
                continue
            keys.append(key)
            if cfg.limit and len(keys) >= cfg.limit:
                return keys
        if not truncated:
            return keys


def pick_host(cfg: Config, seq: int, rng: random.Random) -> str:
    if cfg.host_select == "random":
        return rng.choice(cfg.hosts)
    return cfg.hosts[seq % len(cfg.hosts)]


def fetch_one(cfg: Config, seq: int, key: str, host: str) -> Result:
    start = time.perf_counter()
    status = 0
    bytes_read = 0
    ttfb_ms = 0.0
    err = ""
    try:
        req = signed_request(cfg, "GET", host, f"/{cfg.bucket}/{key}")
        with urllib.request.urlopen(req, timeout=cfg.timeout) as resp:
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
    except Exception as exc:  # noqa: BLE001 - diagnostic script should record all failures.
        err = repr(exc)
    total_ms = (time.perf_counter() - start) * 1000.0
    return Result(seq, key, host, status, bytes_read, ttfb_ms, total_ms, err)


def percentile(values: list[float], pct: float) -> float:
    if not values:
        return 0.0
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


def write_results(path: str, results: list[Result]) -> None:
    with open(path, "w", newline="") as fh:
        writer = csv.writer(fh)
        writer.writerow(
            ["seq", "key", "host", "status", "bytes", "ttfb_ms", "total_ms", "error"]
        )
        for r in sorted(results, key=lambda item: item.seq):
            writer.writerow(
                [
                    r.seq,
                    r.key,
                    r.host,
                    r.status,
                    r.bytes_read,
                    f"{r.ttfb_ms:.3f}",
                    f"{r.total_ms:.3f}",
                    r.error,
                ]
            )


def run(cfg: Config) -> int:
    keys = list_keys(cfg)
    if not keys:
        print("no matching objects found", file=sys.stderr)
        return 2
    if cfg.shuffle:
        random.Random(cfg.seed).shuffle(keys)
    if cfg.write_keys:
        with open(cfg.write_keys, "w") as fh:
            for key in keys:
                fh.write(key + "\n")

    requests: list[str] = []
    if cfg.repeat_mode == "passes":
        for _ in range(cfg.repeat):
            requests.extend(keys)
    else:
        for key in keys:
            requests.extend([key] * cfg.repeat)

    print(
        f"listed={len(keys)} bucket={cfg.bucket} hosts={','.join(cfg.hosts)} "
        f"requests={len(requests)} repeat={cfg.repeat} repeat_mode={cfg.repeat_mode} "
        f"concurrency={cfg.concurrency} shuffle={cfg.shuffle}",
        flush=True,
    )

    rng = random.Random(cfg.seed + 1)
    lock = threading.Lock()
    completed = 0
    results: list[Result] = []
    started = time.perf_counter()

    with concurrent.futures.ThreadPoolExecutor(max_workers=cfg.concurrency) as pool:
        futures = []
        for seq, key in enumerate(requests):
            host = pick_host(cfg, seq, rng)
            futures.append(pool.submit(fetch_one, cfg, seq, key, host))
        for future in concurrent.futures.as_completed(futures):
            result = future.result()
            with lock:
                results.append(result)
                completed += 1
                if cfg.progress_every and completed % cfg.progress_every == 0:
                    elapsed = time.perf_counter() - started
                    print(
                        f"progress={completed}/{len(requests)} "
                        f"rate={completed / elapsed:.2f} obj/s",
                        flush=True,
                    )

    elapsed = time.perf_counter() - started
    write_results(cfg.output, results)
    ok = [r for r in results if 200 <= r.status < 300 and not r.error]
    failed = len(results) - len(ok)
    totals = [r.total_ms for r in ok]
    ttfb = [r.ttfb_ms for r in ok]
    bytes_total = sum(r.bytes_read for r in ok)

    print(f"output={cfg.output}")
    print(
        f"completed={len(results)} ok={len(ok)} failed={failed} "
        f"elapsed={elapsed:.3f}s rate={len(ok) / elapsed:.2f} obj/s "
        f"throughput={bytes_total / elapsed / 1024 / 1024:.2f} MiB/s"
    )
    if totals:
        print(
            "total_ms "
            f"avg={statistics.mean(totals):.3f} "
            f"p50={percentile(totals, 50):.3f} "
            f"p80={percentile(totals, 80):.3f} "
            f"p90={percentile(totals, 90):.3f} "
            f"p95={percentile(totals, 95):.3f} "
            f"p99={percentile(totals, 99):.3f} "
            f"max={max(totals):.3f}"
        )
        print(
            "ttfb_ms "
            f"avg={statistics.mean(ttfb):.3f} "
            f"p50={percentile(ttfb, 50):.3f} "
            f"p80={percentile(ttfb, 80):.3f} "
            f"p90={percentile(ttfb, 90):.3f} "
            f"p95={percentile(ttfb, 95):.3f} "
            f"p99={percentile(ttfb, 99):.3f} "
            f"max={max(ttfb):.3f}"
        )
    return 1 if failed else 0


def parse_args() -> Config:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host", required=True, help="Comma-separated host:port list")
    parser.add_argument("--bucket", default="warp-benchmark-bucket")
    parser.add_argument("--access-key", default="buckitadmin")
    parser.add_argument("--secret-key", default="buckitadmin")
    parser.add_argument("--region", default="us-east-1")
    parser.add_argument("--scheme", default="http", choices=["http", "https"])
    parser.add_argument("--prefix", default="")
    parser.add_argument("--suffix", default=".rnd")
    parser.add_argument("--concurrent", type=int, default=32)
    parser.add_argument("--limit", type=int, default=0, help="0 means all listed keys")
    parser.add_argument("--timeout", type=float, default=30.0)
    parser.add_argument("--shuffle", action="store_true")
    parser.add_argument("--seed", type=int, default=1)
    parser.add_argument(
        "--keys-file",
        default="",
        help="Read object keys from this newline-delimited file instead of listing S3",
    )
    parser.add_argument(
        "--write-keys",
        default="",
        help="Write the final ordered key list to this newline-delimited file",
    )
    parser.add_argument(
        "--host-select",
        choices=["roundrobin", "random"],
        default="roundrobin",
    )
    parser.add_argument(
        "--repeat",
        type=int,
        default=1,
        help="Number of GETs to issue for each listed key",
    )
    parser.add_argument(
        "--repeat-mode",
        choices=["adjacent", "passes"],
        default="adjacent",
        help="adjacent issues key,key before the next key; passes repeats the full list",
    )
    parser.add_argument("--output", default="once-get-results.csv")
    parser.add_argument("--progress-every", type=int, default=500)
    args = parser.parse_args()
    hosts = [host.strip() for host in args.host.split(",") if host.strip()]
    if not hosts:
        parser.error("--host must include at least one host")
    if args.concurrent < 1:
        parser.error("--concurrent must be >= 1")
    if args.repeat < 1:
        parser.error("--repeat must be >= 1")
    return Config(
        scheme=args.scheme,
        hosts=hosts,
        bucket=args.bucket,
        access_key=args.access_key,
        secret_key=args.secret_key,
        region=args.region,
        prefix=args.prefix,
        suffix=args.suffix,
        concurrency=args.concurrent,
        limit=args.limit,
        timeout=args.timeout,
        shuffle=args.shuffle,
        seed=args.seed,
        host_select=args.host_select,
        output=args.output,
        keys_file=args.keys_file,
        write_keys=args.write_keys,
        repeat=args.repeat,
        repeat_mode=args.repeat_mode,
        progress_every=args.progress_every,
    )


if __name__ == "__main__":
    raise SystemExit(run(parse_args()))
