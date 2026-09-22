#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
if [[ -z "${DEPS:-}" ]]; then
  DEPS="$ROOT/.prefix"
fi
if ! grep -q 'kvspaceWriteInPlace' "$DEPS/include/kvspace/kvspace.h" 2>/dev/null; then
  echo "missing kvspaceWriteInPlace in $DEPS/include/kvspace/kvspace.h (set DEPS)" >&2
  exit 1
fi
HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="$HERE/.run"
mkdir -p "$OUT"

export IOPS_N="${IOPS_N:-1000000}"
if [[ -z "${KVSPACE_BACKEND_PATH:-}" && -d "$DEPS/lib/kvspace" ]]; then
  export KVSPACE_BACKEND_PATH="$DEPS/lib/kvspace"
fi
LIBS="$DEPS/lib"
if [[ -n "${KVSPACE_BACKEND_PATH:-}" ]]; then
  LIBS="$LIBS:$KVSPACE_BACKEND_PATH"
fi
export LD_LIBRARY_PATH="$LIBS:$ROOT/bin${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
if [[ -z "${KVSPACE:-}" ]]; then
  export KVSPACE="shm://${OUT}/kv.shm"
fi
rm -f "${OUT}/kv.shm" "${OUT}/kv.shm.sbo.head" "${OUT}/kv.shm.sbo.data"
echo "IOPS_N=$IOPS_N KVSPACE=$KVSPACE"

rustc -O -o "$OUT/loop-rust" "$HERE/loop.rs"
cc -O2 -o "$OUT/loop-kv" "$HERE/kv.c" \
  -I "$DEPS/include" -L "$DEPS/lib" -lkvspace -ldl

"$OUT/loop-rust"
python3 "$HERE/loop.py"
"$OUT/loop-kv"
