# IOPS floor (#204)

Same `a = a + 1` loop, three implementations, shm kvspace for the C path.

```
IOPS_N=1000000 ./bench/iops/run.sh           # default
IOPS_N=100000000 ./bench/iops/run.sh         # issue-sized
```

kvspace path is Get → DecodeHead → int64 +1 → NewInt64 → WriteInPlace（同长）/ WriteNewPlace on key `/a`.
That is the KV round-trip floor, not the kvlang interpreter.

Frozen before numbers: `bench/baseline.md`.
