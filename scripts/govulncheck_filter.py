#!/usr/bin/env python3

import json
import os
import shutil
import subprocess
import sys
from collections import Counter

ALLOWED_OSVS = {
    "GO-2026-5662",
}


def parse_stream(data: str):
    decoder = json.JSONDecoder()
    idx = 0
    size = len(data)
    while idx < size:
        while idx < size and data[idx].isspace():
            idx += 1
        if idx >= size:
            break
        obj, next_idx = decoder.raw_decode(data, idx)
        yield obj
        idx = next_idx


def find_govulncheck() -> str | None:
    govulncheck = shutil.which("govulncheck")
    if govulncheck is not None:
        return govulncheck

    gobin = subprocess.run(
        ["go", "env", "GOBIN"],
        capture_output=True,
        text=True,
        check=True,
    ).stdout.strip()
    if gobin:
        candidate = os.path.join(gobin, "govulncheck")
        if os.path.exists(candidate):
            return candidate

    gopath = subprocess.run(
        ["go", "env", "GOPATH"],
        capture_output=True,
        text=True,
        check=True,
    ).stdout.strip()
    candidate = os.path.join(gopath, "bin", "govulncheck")
    if os.path.exists(candidate):
        return candidate

    return None


def main() -> int:
    govulncheck = find_govulncheck()
    if govulncheck is None:
        sys.stderr.write("govulncheck binary not found in PATH, GOBIN, or GOPATH/bin\n")
        return 1

    proc = subprocess.run(
        [govulncheck, "-format", "json", "./..."],
        capture_output=True,
        text=True,
        check=False,
    )

    if proc.stderr:
        sys.stderr.write(proc.stderr)

    findings = Counter()
    summaries = {}
    urls = {}

    try:
        for obj in parse_stream(proc.stdout):
            osv = obj.get("osv")
            if isinstance(osv, dict):
                osv_id = osv.get("id")
                if isinstance(osv_id, str):
                    summaries[osv_id] = osv.get("summary", "")
                    db = osv.get("database_specific") or {}
                    if isinstance(db, dict):
                        urls[osv_id] = db.get("url", "")
                continue

            finding = obj.get("finding")
            if isinstance(finding, dict):
                osv_id = finding.get("osv")
                if isinstance(osv_id, str):
                    findings[osv_id] += 1
    except json.JSONDecodeError as exc:
        sys.stderr.write(f"failed to parse govulncheck JSON: {exc}\n")
        if proc.stdout:
            sys.stderr.write(proc.stdout)
        return proc.returncode or 1

    if proc.returncode not in (0, 3):
        if proc.stdout:
            sys.stderr.write(proc.stdout)
        return proc.returncode

    blocked = sorted(osv for osv in findings if osv not in ALLOWED_OSVS)
    ignored = sorted(osv for osv in findings if osv in ALLOWED_OSVS)

    if ignored:
        for osv in ignored:
            summary = summaries.get(osv, "").strip()
            url = urls.get(osv, "").strip()
            print(f"ignoring known unfixed advisory {osv}: {summary}")
            if url:
                print(url)
            print(f"reachable findings: {findings[osv]}")

    if blocked:
        print("govulncheck found reachable vulnerabilities outside the allowlist:")
        for osv in blocked:
            summary = summaries.get(osv, "").strip()
            url = urls.get(osv, "").strip()
            print(f"- {osv}: {summary}")
            if url:
                print(f"  {url}")
            print(f"  reachable findings: {findings[osv]}")
        return 1

    if findings:
        print("govulncheck found only allowlisted reachable vulnerabilities.")
    else:
        print("govulncheck found no reachable vulnerabilities.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
