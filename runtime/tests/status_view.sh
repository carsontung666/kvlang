#!/usr/bin/env bash
# P1b4-b1a：status 只读 slot view 路径（shm）必须观测外部 paused/running/done/error。
# 复用 pause_resume 的全流程，只把后端固定为 shm（view 生效路径）。
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
PAUSE_RESUME_DSN="shm://$tmp/sv" \
KVLANG_LAYOUT_BIN="${KVLANG_LAYOUT_BIN:-$ROOT/bin/kvlanglayout}" \
KVLANG_TERM_BIN="${KVLANG_TERM_BIN:-$ROOT/bin/byteseek}" \
KVSPACE_CLI="${KVSPACE_CLI:-kvspace}" \
	bash "$ROOT/runtime/tests/pause_resume.sh" || exit 1

# Also verify fast-kernel tight loop (no kvspace.set inside loop) observes external paused/done immediately.
LAYOUT="${KVLANG_LAYOUT_BIN:-$ROOT/bin/kvlanglayout}"
KVLANG="${KVLANG_TERM_BIN:-$ROOT/bin/byteseek}"
KVC="${KVSPACE_CLI:-kvspace}"
export KVSPACE="shm://$tmp/fk"
cat > "$tmp/fk_loop.kv" <<'EOF'
rwfunc spin() -> () {
	i = 0
	while (true) {
		i = i + 1
	}
}
EOF
"$KVLANG" -c 'print("boot")' >/dev/null || exit 1
"$LAYOUT" "$tmp/fk_loop.kv" "$KVSPACE" >/dev/null 2>&1 || exit 1
vid=$("$KVLANG" create spin) || exit 1
STKEY=$(printf '/vthread/%s/\xe2\x80\xa5status' "$vid")
"$KVLANG" run "$vid" >/dev/null 2>&1 &
RPID=$!
sleep 0.1
"$KVC" set "$STKEY" "string:paused" >/dev/null || exit 1
for _ in $(seq 1 100); do
	kill -0 "$RPID" 2>/dev/null || break
	sleep 0.05
done
if kill -0 "$RPID" 2>/dev/null; then
	kill -9 "$RPID" 2>/dev/null
	echo "FAIL: fast-kernel spin loop ignored external paused" >&2
	exit 1
fi
echo "PASS: fast-kernel spin loop observed external paused"
