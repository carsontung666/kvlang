#!/usr/bin/env python3
import os
import time

n = int(os.environ.get("IOPS_N", "1000000"))
a = 0
t0 = time.perf_counter_ns()
while a < n:
    a = a + 1
t1 = time.perf_counter_ns()
if a != n:
    raise SystemExit(f"python: a={a} want {n}")
ns = t1 - t0
print(f"python n={n} ns={ns} ns/iter={ns / n:.3f} a={a}")
