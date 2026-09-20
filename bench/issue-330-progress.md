# Issue progress: #330 (v0.3) and #329

Logged 2026-09-20 on `feat/issue-330-cp`. GitHub issues are still **OPEN**. This file is a local status dump, not a close-out.

Numbers vs `bench/baseline.md` (2026-09-16, same machine). After numbers also in `bench/v0.3.md`.

Env: `KVSPACE_BACKEND_PATH=/home/junyao/code/kvspace-c/build-pr330` for shm; durable redis/fs use `bin/kvspace/libkvspace_durable.so`. PC is still a kvspace path (`VTHREAD_ROOT/<vtid>/·pc`).

---

## GitHub

| issue | title | GitHub | this tree |
|-------|--------|--------|-----------|
| [#330](https://github.com/carsontung666/kvlang/issues/330) | [Roadmap] v0.3：性能提升 + rwir/stdlib 优化 | OPEN | 完成判据 A met; B partial; C 2/3 backends |
| [#329](https://github.com/carsontung666/kvlang/issues/329) | [RFC] XValue head 重定 | OPEN | not implemented (RFC undecided) |

#330 完成判据 (from the issue):

- **A**: iops / prime_sieve have reproducible improvement; PC stays a KV path string
- **B**: stdlib covers native rwir; new extensions do not require core changes
- **C**: tutorial green on three backends

---

## A. Performance

### prime_sieve(200) — #194

| impl | baseline | after |
|------|----------|-------|
| kvlang | **6178.832 ms** | **12.45 ms** (~496×) |
| Python | 0.150 ms | 0.167 ms |
| C `-O3` | 0.034 ms | 0.038 ms |

kvlang / Python: ~41200× → ~75×. Official `tutorial/test.py --bench` on shm: kvlang **14.075 ms**, VALID:1 INVALID:0 SKIP:236.

### IOPS — #204

`IOPS_N=1000000 ./bench/iops/run.sh`

| impl | baseline ns/iter | after ns/iter |
|------|------------------|---------------|
| Python `a=a+1` | 47.079 | 54.498 |
| kvspace-c Get+Set | **172.857** | **172.356** |
| kvspace-c in-place body | — | **30.775** |

N=1e8 Get+Set floor in baseline: **168.938 ns/iter**. Same-length in-place is ~5.6× that floor. The Get+Set microbench itself did not move; interpreter sieve did.

### What landed (A-line code)

- Default shm Get is a borrowed view (no `KVLANG_SHM_BORROW` gate); same-length in-place body write is visible through a held view. `runtime/tests/test_kv_cp.c`: 0 FAIL.
- `kvspaceCp` independence: overwrite src after cp leaves dst unchanged.
- IR decode GetBatch chunked (stop on empty chunk) instead of 257 slots per op.
- Hot path: cache ART nid+gen; Get prefers `ShmGetByRef`; same-length Set uses `ShmSetPartByRef`; first Set of a key uses `ShmSet` (64-byte box, not frontend 24-byte TLV).
- GetByRef miss does not fall back to extindex ShmGet (no `/lib` punch-through). `Del` drops the nid cache.
- Empty/None Set deletes the key so `kv·get != None` does not reuse a stale slot (`242_valid_anagram.kv`).
- kvspace-c: wire-length Get (not sbo slot size); List falls through to ART scan when memindex is not a 3-axis matrix; `add_child` does not clobber kvlang newline memindex.

### A child issues (roadmap list)

| child | status here |
|-------|-------------|
| #212 shm zero-copy / Get-Set | partial — borrowed Get + in-place Set on shm |
| #204 IOPS floor | measured; Get+Set ~170 ns; in-place ~31 ns |
| #194 prime_sieve | improved, still ~75× Python |
| #255 opcode id intern | present (`kvlangOpcodeIntern`); not full three-table quickening |
| #268 ByRef | ResolveRef / GetByRef / SetPartByRef wired on shm |
| #267 art_scan | used in kvspace-c List |
| #269 stdlib pre-layout | boot skips re-layout if `/lib/math·Pi` exists |
| #61 relative paths | not done |
| #203 PC vs CPython | not done |
| #329 XValue head RFC | not done (see below) |

---

## B. rwir / stdlib

Landed this round:

- `kv·cp` / `kv·cpdir` (#200) — `kvlangKvCp` / `kvlangKvCpTree`; tutorials `tutorial/13-stdlib/kv/cp.kv` and `cpdir.kv`.

Already in tree before this log (stdlib + native rwir table): string, time/duration, xv, kv get/set/list, networld proc/fs, vthread create/run/call.

Not done: #166 op-gpu, #165 cbindgen/uniffi, #51 kv.find/grep.

---

## C. Tutorial / backends

SKIP 3 on every backend: `tutorial/12-struct/{01-linked-list,02-tree,03-graph}.kv` (`// extern`, #199).

| backend | PASS | FAIL | SKIP | notes |
|---------|------|------|------|-------|
| shm kvspace-c | **187** | **0** | 3 | `python3 tutorial/test.py --kvspace shm://…` |
| fs:// durable | **187** | **0** | 3 | `KVSPACE_BACKEND_PATH=bin/kvspace` |
| redis durable :6380 | 167 | **20** | 3 | scatter List, `walk_lib`, some maps — durable-redis memindex; no durable source in this workspace |

#251 (shm tutorial incomplete) is cleared on shm (was 10/18 historically; now 187/0/3). Redis 20 FAIL is the remaining C gap.

---

## #329 RFC

Still OPEN. Five 待裁决 items are empty. Local kvspace-c uses a 64-byte 3-axis head; that is **not** the RFC (ref int8 / storetype → (bodycap,valuelen) / fixed langtype). No breaking cross-repo tag.

---

## Still blocking a GitHub close of #330

- Redis tutorial 20 FAIL (durable, not this runtime)
- #329 RFC undecided
- #199 struct (the 3 SKIP files)
- #166 GPU, #165 bindings, #51 find/grep
- Pointer language: #283 #285 #286 #305
- #291 / #335 multi-assign
- #61 relative paths, #203 CPython comparison

Do not close #330 or #329 on GitHub from this log.
