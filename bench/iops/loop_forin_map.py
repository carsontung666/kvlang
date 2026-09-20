#!/usr/bin/env python3
import time

L = 32
n = 50000
a = [1] * L
k = 0
acc = 0
t0 = time.perf_counter_ns()
while k < n:
    for x in a:
        acc = acc + x
    k = k + 1
t1 = time.perf_counter_ns()
print("shape=forin-map")
print(f"python-forin-map n={n} ns={t1 - t0} acc={acc} k={k}")
