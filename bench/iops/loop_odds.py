#!/usr/bin/env python3
import os
import time

n = int(os.environ.get("IOPS_N", "1000000"))
total = 0
i = 1
t0 = time.perf_counter_ns()
while i <= n:
    rem = i % 2
    is_odd = rem == 1
    if is_odd:
        total = total + i
    i = i + 1
t1 = time.perf_counter_ns()
print("shape=rem/eq/if")
print(f"python-odds n={n} ns={t1 - t0} ns/iter={(t1 - t0) / n:.3f} total={total} i={i}")
