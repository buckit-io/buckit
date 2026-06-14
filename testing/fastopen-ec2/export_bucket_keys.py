#!/usr/bin/env python3
"""Export current S3 object keys from a bucket."""

from __future__ import annotations

import argparse
from pathlib import Path

import boto3
from botocore.config import Config


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--endpoint", required=True, help="S3 endpoint, e.g. http://127.0.0.1:9000")
    p.add_argument("--bucket", required=True)
    p.add_argument("--access-key", default="buckitadmin")
    p.add_argument("--secret-key", default="buckitadmin")
    p.add_argument("--region", default="us-east-1")
    p.add_argument("--output", required=True, help="Path to write newline-delimited keys")
    p.add_argument("--prefix", default="", help="Optional key prefix filter")
    return p.parse_args()


def main() -> int:
    args = parse_args()
    client = boto3.client(
        "s3",
        endpoint_url=args.endpoint,
        aws_access_key_id=args.access_key,
        aws_secret_access_key=args.secret_key,
        region_name=args.region,
        config=Config(signature_version="s3v4"),
    )

    out = Path(args.output)
    out.parent.mkdir(parents=True, exist_ok=True)

    count = 0
    paginator = client.get_paginator("list_objects_v2")
    with out.open("w", encoding="utf-8") as fh:
        for page in paginator.paginate(
            Bucket=args.bucket,
            Prefix=args.prefix,
            PaginationConfig={"PageSize": 1000},
        ):
            for obj in page.get("Contents", []):
                fh.write(obj["Key"] + "\n")
                count += 1

    print(f"keys_file={out}")
    print(f"key_count={count}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
