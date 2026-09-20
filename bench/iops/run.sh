#!/usr/bin/env bash
# IOPS floor (#204): Rust / Python / kvspace-c shm, same a=a+1 loop.
#   IOPS_N=1000000 ./bench/iops/run.sh          # default (CI-sized)
#   IOPS_N=100000000 ./bench/iops/run.sh        # issue-sized (slow)
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
DEPS="${DEPS:-$ROOT/.deps}"
HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="$HERE/.run"
mkdir -p "$OUT"
KVC="${KVC:-}"
if [[ -z "$KVC" ]]; then
  if [[ -d "$ROOT/../kvspace-c/build-head64" ]]; then
    KVC="$ROOT/../kvspace-c/build-head64"
  elif [[ -d "$ROOT/../kvspace-c/build-pr330" ]]; then
    KVC="$ROOT/../kvspace-c/build-pr330"
  fi
fi
if [[ -z "${KVSPACE_BACKEND_PATH:-}" ]]; then
  if [[ -n "$KVC" ]]; then
    export KVSPACE_BACKEND_PATH="$KVC"
  else
    export KVSPACE_BACKEND_PATH="${DEPS}/lib/kvspace"
  fi
fi
export IOPS_N="${IOPS_N:-1000000}"
export LD_LIBRARY_PATH="${DEPS}/lib:${KVSPACE_BACKEND_PATH}:${ROOT}/bin${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
if [[ -z "${KVSPACE:-}" ]]; then
  export KVSPACE="shm://${OUT}/kv.shm"
fi
rm -f "${OUT}/kv.shm" "${OUT}/kv.shm.sbo.head" "${OUT}/kv.shm.sbo.data"
echo "IOPS_N=$IOPS_N KVSPACE=$KVSPACE KVSPACE_BACKEND_PATH=$KVSPACE_BACKEND_PATH KVC=$KVC"

rustc -O -o "$OUT/loop-rust" "$HERE/loop.rs"
cc -O2 -o "$OUT/loop-kv" "$HERE/kv.c" \
  -I "$DEPS/include" -L "$DEPS/lib" -lkvspace -ldl

if [[ -n "$KVC" && -f "$KVC/libkvspace-c.so" ]]; then
  cc -O2 -o "$OUT/loop-kv-inplace" "$HERE/kv_inplace.c" \
    -I "$ROOT/../kvspace-c/src" -I "$DEPS/include" \
    -L "$KVC" -lkvspace-c -Wl,-rpath,"$KVC" \
    -L "$DEPS/lib" -Wl,-rpath,"$DEPS/lib"
fi

"$OUT/loop-rust"
python3 "$HERE/loop.py"
"$OUT/loop-kv"
if [[ -x "$OUT/loop-kv-inplace" ]]; then
  "$OUT/loop-kv-inplace"
fi
