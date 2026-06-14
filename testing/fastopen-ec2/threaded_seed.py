#!/usr/bin/env python3
"""Parallel deterministic S3 object seeder for two-host FastOpen rigs."""

from __future__ import annotations

import argparse
import concurrent.futures as cf
import sys
import time

import boto3
from botocore.config import Config


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description="Seed deterministic objects with a threaded uploader.",
    )
    p.add_argument("--endpoint", default="http://127.0.0.1:9000")
    p.add_argument("--bucket", default="fastopen-bench")
    p.add_argument("--access-key", default="buckitadmin")
    p.add_argument("--secret-key", default="buckitadmin")
    p.add_argument("--region", default="us-east-1")
    p.add_argument("--key-prefix", required=True)
    p.add_argument("--start-index", type=int, required=True)
    p.add_argument("--object-count", type=int, required=True)
    p.add_argument("--object-size-bytes", type=int, default=655360)
    p.add_argument("--workers", type=int, default=32)
    p.add_argument("--progress-every", type=int, default=100)
    return p.parse_args()


def main() -> int:
    args = parse_args()
    client = boto3.client(
        "s3",
        endpoint_url=args.endpoint,
        aws_access_key_id=args.access_key,
        aws_secret_access_key=args.secret_key,
        region_name=args.region,
        config=Config(
            max_pool_connections=args.workers + 8,
            retries={"max_attempts": 5},
        ),
    )
    try:
        client.create_bucket(Bucket=args.bucket)
    except Exception:
        pass

    body = b"x" * args.object_size_bytes
    completed = 0
    started = time.time()

    def put_one(idx: int) -> int:
        key = f"{args.key_prefix}{idx:06d}"
        client.put_object(Bucket=args.bucket, Key=key, Body=body)
        return idx

    with cf.ThreadPoolExecutor(max_workers=args.workers) as ex:
        futs = [
            ex.submit(put_one, idx)
            for idx in range(args.start_index, args.start_index + args.object_count)
        ]
        for fut in cf.as_completed(futs):
            fut.result()
            completed += 1
            if completed % args.progress_every == 0 or completed == args.object_count:
                elapsed = time.time() - started
                rate = completed / elapsed if elapsed else 0.0
                print(
                    f"completed={completed}/{args.object_count} rate={rate:.2f}/s",
                    flush=True,
                )
    return 0


if __name__ == "__main__":
    sys.exit(main())
