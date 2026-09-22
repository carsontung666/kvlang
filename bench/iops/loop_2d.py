#!/usr/bin/env python3
import time

n = 20000
m = [[0, 0, 0, 0], [0, 0, 0, 0]]
t0 = time.perf_counter_ns()
k = 0
while k < n:
    i = 0
    while i < 2:
        j = 0
        while j < 4:
            m[i][j] = 1
            j = j + 1
        i = i + 1
    k = k + 1
t1 = time.perf_counter_ns()
acc = 0
i = 0
while i < 2:
    j = 0
    while j < 4:
        acc = acc + m[i][j]
        j = j + 1
    i = i + 1
print("shape=arraynd-2d")
print(f"python-2d n={n} ns={t1 - t0} ns/iter={(t1 - t0) / n:.3f} acc={acc}")
