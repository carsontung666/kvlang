#!/usr/bin/env python3
import time

a = 0.0
n = 1000000.0
t0 = time.perf_counter_ns()
while a < n:
    a = a + 1.0
t1 = time.perf_counter_ns()
print("shape=float64-inc")
print(f"python-f64 n={n} ns={t1 - t0} ns/iter={(t1 - t0) / n:.3f} a={a}")
