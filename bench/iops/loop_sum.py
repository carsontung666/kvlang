#!/usr/bin/env python3
import os
import time

n = int(os.environ.get("IOPS_N", "1000000"))
acc = 0
i = 1
t0 = time.perf_counter_ns()
while i <= n:
    acc = acc + i
    i = i + 1
t1 = time.perf_counter_ns()
print(f"python-sum n={n} ns={t1 - t0} ns/iter={(t1 - t0) / n:.3f} acc={acc} i={i}")
