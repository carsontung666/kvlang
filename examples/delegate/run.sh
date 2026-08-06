#!/bin/sh
# 委托 rwir 的跨进程验收：起临时 redis → 起外部执行器 → 跑 echo.kv → 比对输出 → 全部清理。
#
# 为什么需要它：go test 里的执行器是同进程 goroutine（art:// 是进程内 kvspace），
# 它验证的是 VM 侧协议。而 examples/ 这套是用户真会跑的东西——CLI 的值编码、
# 参数顺序、键布局——只有真起两个进程才验得到。协议改过而这里没跑，就会悄悄烂掉。
#
#   sh examples/delegate/run.sh
#
# 需要 redis-server 与 kvspace CLI 在 PATH 上，或用环境变量指定：
#   REDIS_SERVER=/path/to/redis-server KVSPACE_CLI=/path/to/kvspace sh examples/delegate/run.sh
set -e

PORT="${PORT:-63799}"
REDIS_SERVER="${REDIS_SERVER:-redis-server}"
KVSPACE_CLI="${KVSPACE_CLI:-kvspace}"
DSN="redis://127.0.0.1:$PORT"
DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT=$(CDPATH= cd -- "$DIR/../.." && pwd)
TMP=$(mktemp -d)

cleanup() {
	[ -f "$TMP/be.pid" ] && kill "$(cat "$TMP/be.pid")" 2>/dev/null || true
	"$REDIS_SERVER" --version >/dev/null 2>&1 && \
		(echo shutdown nosave | "${REDIS_CLI:-redis-cli}" -p "$PORT" >/dev/null 2>&1 || true)
	rm -rf "$TMP"
}
trap cleanup EXIT INT TERM

command -v "$REDIS_SERVER" >/dev/null 2>&1 || { echo "缺少 redis-server（art:// 不行，外部进程看不见进程内 kvspace）"; exit 1; }
command -v "$KVSPACE_CLI" >/dev/null 2>&1 || { echo "缺少 kvspace CLI：go build -o kvspace github.com/array2d/kvspace-go/cmd/kvspace"; exit 1; }

[ -x "$ROOT/kvlang" ] || (cd "$ROOT" && go build -o kvlang ./cmd/kvlang/)

"$REDIS_SERVER" --port "$PORT" --save '' --appendonly no --daemonize yes \
	--pidfile "$TMP/r.pid" --logfile "$TMP/r.log"
sleep 1

export KVLANG_KVSPACE="$DSN" KVSPACE_CLI="$KVSPACE_CLI"
python3 "$DIR/fake_backend.py" > "$TMP/be.log" 2>&1 &
echo $! > "$TMP/be.pid"
sleep 2

"$ROOT/kvlang" "$DIR/echo.kv" > "$TMP/out.txt" 2>&1 || {
	echo "FAIL: kvlang 退出非零"; cat "$TMP/out.txt"; cat "$TMP/be.log"; exit 1
}

# 期望输出取自 echo.kv 头部的 `#   ` 注释块，与 tutorial/test.py 同一约定
sed -n '/^# 期望输出:/,/^#$/p' "$DIR/echo.kv" | sed -n 's/^#   //p' > "$TMP/want.txt"
if diff -u "$TMP/want.txt" "$TMP/out.txt" > "$TMP/diff.txt"; then
	echo "PASS: 跨进程委托输出与期望一致"
	cat "$TMP/out.txt"
else
	echo "FAIL: 输出不符"; cat "$TMP/diff.txt"; echo "--- 后端日志 ---"; cat "$TMP/be.log"; exit 1
fi
