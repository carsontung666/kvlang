#!/usr/bin/env python3
import time

N = 400
acc = 0
n = 0
t0 = time.perf_counter_ns()
while n < N:
    i = 0
    while i < N:
        acc = acc + 1
        i = i + 1
    n = n + 1
t1 = time.perf_counter_ns()
print("shape=nested-while")
print(f"python-nested N={N} ns={t1 - t0} ns/iter={(t1 - t0) / (N * N):.3f} acc={acc} n={n}")
