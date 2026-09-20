#!/usr/bin/env python3
import time

limit = 200
count = 0
n = 2
t0 = time.perf_counter_ns()
while n <= limit:
    is_prime = True
    d = 2
    while d < n:
        rem = n % d
        divisible = rem == 0
        if divisible:
            is_prime = False
            break
        else:
            d = d + 1
    if is_prime:
        count = count + 1
    n = n + 1
t1 = time.perf_counter_ns()
print("shape=sieve-noprint")
print(f"python-sieve limit={limit} ns={t1 - t0} count={count}")
