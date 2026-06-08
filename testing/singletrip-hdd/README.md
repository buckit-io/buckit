# Single-Trip HDD Bench Rig

This directory is the checked-in source of truth for the ad hoc HDD benchmark rig used for phase-1 single-trip GET evaluation.

It replaces the temporary host-only copies that were previously stored under `/root/singletrip-bench/` on `192.184.90.116`.

## Files

- `start-buckit.sh`
  - starts Buckit with a supplied list of local data paths
  - enables or disables the fast path with `FAST_GET=0|1`
- `bench-warp-2m.sh`
  - runs the saturated `warp get` 2 MiB comparison used for the earlier Docker and host pilots
- `cold-curl-ttfb.sh`
  - runs the host-local cold TTFB test over a list of presigned URLs
  - drops page cache before each GET
  - prints one `curl` TTFB value per request

## Expected Host Layout

Default host workspace:

- root dir: `/root/singletrip-bench`
- Buckit binary: `/root/singletrip-bench/bin/buckit`
- Warp binary: `/root/singletrip-bench/bin/warp`

You can copy this directory to the host as-is and then copy or build the binaries into `bin/`.

## Start Buckit

`FAST_GET=1`:

```sh
FAST_GET=1 ./start-buckit.sh /mnt/data01 /mnt/data02 /mnt/data03 /mnt/data04
```

`FAST_GET=0`:

```sh
FAST_GET=0 ./start-buckit.sh /mnt/data01 /mnt/data02 /mnt/data03 /mnt/data04
```

Notes:

- `MINIO_CI_CD=1` may be required when using root-disk paths for a control rig.
- Default credentials are `buckitadmin/buckitadmin`.

## Warp Flow

Load once with fast path on:

```sh
./bench-warp-2m.sh load-on
```

Then restart the server with fast path off and run:

```sh
./bench-warp-2m.sh off
```

Then restart the server with fast path on again and run:

```sh
./bench-warp-2m.sh on
```

## Cold TTFB Flow

1. Generate presigned URLs for the objects you want to probe.
2. Run `mc admin trace --verbose` from another shell.
3. From the host, run:

```sh
./cold-curl-ttfb.sh "<url-1>" "<url-2>" "<url-3>"
```

This prints one `curl` `time_starttransfer` value per GET.

## Current Notes

The current single-HDD control results and interpretation are documented in:

- `docs/single-trip-hdd-bench/README.md`

That document records the June 4-5, 2026 single-disk control run, including:

- the `2 MiB` positive signal
- the noisier `640 KiB` runs
- the path dependency checks

The two-node real-HDD (`d3.xlarge`) cold-TTFB A/B runs are documented separately in:

- `docs/single-trip-hdd-bench/two-node-d3-2mib.md` — June 5, 2026 `2 MiB` run on a
  2-node EC:3 cluster (300 pooled cold samples per arm: ~14% median TTFB reduction
  with the fast path), plus the method corrections that invalidate the earlier
  confounded 2 MiB numbers.
- `docs/single-trip-hdd-bench/two-node-d3-640kib-ec2.md` — June 5, 2026 `640 KiB`
  run with the set reconfigured to EC:2 (4 data + 2 parity). Crossover result: the
  fast path is worse at low/median percentiles (~4 ms floor from cancel-close
  overhead) but better in the tail (p90 −26%, mean −18%).
- `docs/single-trip-hdd-bench/two-node-d3-profiling.md` — June 5, 2026 source-level
  GET phase profiling (`BUCKIT_FASTGET_PROFILE=1`). Finds the bulk of a cold GET is
  cross-node cold disk opens (OFF pays the phase twice; ON's open phase is gated by
  the slowest-of-6 incl. discarded parity, plus a header→body barrier).
