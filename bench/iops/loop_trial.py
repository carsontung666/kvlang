#!/usr/bin/env python3
import time

limit = 500
np = 0
n = 2
t0 = time.perf_counter_ns()
while n <= limit:
    is_prime = True
    d = 2
    while d < n:
        rem = n % d
        if rem == 0:
            is_prime = False
            break
        else:
            d = d + 1
    if is_prime:
        np = np + 1
    n = n + 1
t1 = time.perf_counter_ns()
print("shape=nested-trial")
print(f"python-trial limit={limit} ns={t1 - t0} ns/iter={(t1 - t0) / max(limit, 1):.3f} np={np}")
