# Baseline (do not treat as an improvement)

Frozen **before** v0.3 A-line work. Later numbers must use the same commands on the same machine.

- date: 2026-09-16
- git: `a0b336062c51b4ba5043fb8664e82f33213a286e` plus uncommitted `feat/issue-330-cp` (cp / borrowed Get / sbo TLV reuse / slotsbox obj_boundary)
- host: linux x86_64, 96 nproc
- DSN: `shm://` with `KVSPACE_BACKEND_PATH=$ROOT/../kvspace-c/build-pr330`

## IOPS (#204) `IOPS_N=100000000 ./bench/iops/run.sh`

| impl | ns/iter | total |
|------|---------|-------|
| Rust `rustc -O` + `black_box` | 1.361 | 0.136 s |
| Python 3 `while a < n: a = a+1` | 49.319 | 4.932 s |
| kvspace-c Get+decode / +1 / NewInt64+Set | **168.938** | 16.894 s |

N=1e6 smoke: rust 1.907 / python 47.079 / kvspace-c 172.857 ns/iter.

Issue #204 quoted 695.8 ns/iter on another machine and older shm; that figure is **not** this baseline.

## prime_sieve(200) `python3 tutorial/test.py --no-build --bench --kvspace shm://…`

| impl | ms |
|------|----|
| kvlang `bin/kvlang` | **6178.832** |
| Python | 0.150 |
| C `-O3` | 0.034 |

kvlang / Python ≈ 41200×. Issue #194 quoted 33.37 s kvlang on another machine; that figure is **not** this baseline.

PC remains a kvspace path (`·pc`); this freeze does not add a process-private PC.
