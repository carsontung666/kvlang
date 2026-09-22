# Baseline (do not treat as an improvement)

Frozen **before** v0.3 A-line work. Later numbers must use the same commands on the same machine.

- date: 2026-09-16
- host: linux x86_64, 96 nproc
- DSN: `shm://`

## IOPS (#204) `IOPS_N=100000000 ./bench/iops/run.sh`

| impl | ns/iter | total |
|------|---------|-------|
| Rust `rustc -O` + `black_box` | 1.361 | 0.136 s |
| Python 3 `while a < n: a = a+1` | 49.319 | 4.932 s |
| kvspace Get+decode / +1 / NewInt64+Set | **168.938** | 16.894 s |

N=1e6 smoke: rust 1.907 / python 47.079 / kvspace 172.857 ns/iter.

Issue #204 quoted 695.8 ns/iter on another machine and older shm; that figure is **not** this baseline.
