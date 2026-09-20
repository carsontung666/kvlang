#!/usr/bin/env python3
import time

L = 32
n = 50000
a = [1] * L
k = 0
t0 = time.perf_counter_ns()
while k < n:
    t = 0
    while t < L:
        a[t] = 1
        t = t + 1
    i = 2
    while i < L:
        j = i
        while j < L:
            a[j] = 0
            j = j + i
        i = i + 1
    k = k + 1
t1 = time.perf_counter_ns()
acc = 0
t = 0
while t < L:
    acc = acc + a[t]
    t = t + 1
print("shape=arraynd-stride")
print(f"python-array n={n} ns={t1 - t0} acc={acc} k={k}")
