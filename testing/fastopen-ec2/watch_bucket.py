#!/usr/bin/env python3
"""Watch S3 bucket object/version counts over time."""

from __future__ import annotations

import argparse
import datetime as dt
import sys
import time

import boto3
from botocore.config import Config


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--endpoint", required=True, help="S3 endpoint, e.g. http://127.0.0.1:9000")
    p.add_argument("--bucket", required=True, help="Bucket name")
    p.add_argument("--access-key", default="buckitadmin")
    p.add_argument("--secret-key", default="buckitadmin")
    p.add_argument("--region", default="us-east-1")
    p.add_argument("--interval", type=float, default=60.0, help="Seconds between samples")
    p.add_argument("--samples", type=int, default=0, help="Number of samples to take; 0 means run forever")
    p.add_argument("--versions", action="store_true", help="Also count object versions and delete markers")
    return p.parse_args()


def make_client(args: argparse.Namespace):
    return boto3.client(
        "s3",
        endpoint_url=args.endpoint,
        aws_access_key_id=args.access_key,
        aws_secret_access_key=args.secret_key,
        region_name=args.region,
        config=Config(signature_version="s3v4"),
    )


def count_objects(s3, bucket: str) -> tuple[int, int]:
    count = 0
    size_bytes = 0
    paginator = s3.get_paginator("list_objects_v2")
    for page in paginator.paginate(Bucket=bucket, PaginationConfig={"PageSize": 1000}):
        count += page.get("KeyCount", 0)
        for obj in page.get("Contents", []):
            size_bytes += int(obj.get("Size", 0))
    return count, size_bytes


def count_versions(s3, bucket: str) -> tuple[int, int, int]:
    version_count = 0
    delete_markers = 0
    version_bytes = 0
    paginator = s3.get_paginator("list_object_versions")
    for page in paginator.paginate(Bucket=bucket, PaginationConfig={"PageSize": 1000}):
        versions = page.get("Versions", [])
        markers = page.get("DeleteMarkers", [])
        version_count += len(versions)
        delete_markers += len(markers)
        for version in versions:
            version_bytes += int(version.get("Size", 0))
    return version_count, delete_markers, version_bytes


def main() -> int:
    args = parse_args()
    s3 = make_client(args)

    print(
        "timestamp,current_objects,current_bytes,versions,delete_markers,version_bytes,"
        "delta_objects,delta_bytes,delta_versions,delta_delete_markers,delta_version_bytes",
        flush=True,
    )

    prev = None
    sample = 0
    while True:
        now = dt.datetime.now(dt.UTC).isoformat(timespec="seconds")
        current_objects, current_bytes = count_objects(s3, args.bucket)
        versions = delete_markers = version_bytes = 0
        if args.versions:
            versions, delete_markers, version_bytes = count_versions(s3, args.bucket)

        current = (current_objects, current_bytes, versions, delete_markers, version_bytes)
        if prev is None:
            delta = (0, 0, 0, 0, 0)
        else:
            delta = tuple(cur - old for cur, old in zip(current, prev))

        print(
            f"{now},{current_objects},{current_bytes},{versions},{delete_markers},{version_bytes},"
            f"{delta[0]},{delta[1]},{delta[2]},{delta[3]},{delta[4]}",
            flush=True,
        )

        prev = current
        sample += 1
        if args.samples and sample >= args.samples:
            return 0
        time.sleep(args.interval)


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except KeyboardInterrupt:
        sys.exit(130)
