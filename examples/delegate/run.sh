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

# 默认随机高端口：多次连续运行不会互相干扰（上一次的 redis/后端若尚未完全退出，
# 固定端口会让这一次连到将死的实例或被残留 watcher 抢走任务）。CI 传 PORT=6379 复用 service。
PORT="${PORT:-$(awk "BEGIN{srand();print 40000+int(rand()*20000)}")}"
REDIS_SERVER="${REDIS_SERVER:-redis-server}"
KVSPACE_CLI="${KVSPACE_CLI:-kvspace}"
DSN="redis://127.0.0.1:$PORT"
DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT=$(CDPATH= cd -- "$DIR/../.." && pwd)
TMP=$(mktemp -d)

STARTED_REDIS=""
cleanup() {
	# 先杀后端并**等它真的退出**：它阻塞在 kvspace watch 子进程上，只 kill 不等的话
	# 那个子进程会以孤儿身份继续持有 redis 连接。
	if [ -f "$TMP/be.pid" ]; then
		BE=$(cat "$TMP/be.pid")
		# 顺序要紧：先杀它的 kvspace watch 子进程，再杀它本身。
		# 反过来的话子进程会被 reparent 成孤儿，而 go-redis 客户端会自动重连——
		# 一个"已经杀掉"的后端能在下一次运行时重新连上同一端口并抢走任务。
		pkill -P "$BE" 2>/dev/null || true
		kill "$BE" 2>/dev/null || true
		wait "$BE" 2>/dev/null || true
	fi
	[ -n "$STARTED_REDIS" ] && (echo shutdown nosave | "${REDIS_CLI:-redis-cli}" -p "$PORT" >/dev/null 2>&1 || true)
	rm -rf "$TMP"
}
trap cleanup EXIT INT TERM

command -v "$KVSPACE_CLI" >/dev/null 2>&1 || {
	echo "缺少 kvspace CLI。注意要从 go.mod 钉住的版本构建，不能用 @latest —"
	echo "唯一的 tag v0.1.0 与本仓库 API 及值编码都不同源："
	echo "  go build -o kvspace github.com/array2d/kvspace-go/cmd/kvspace"
	exit 1
}

# 已有 redis 就复用（CI 用 service 容器提供），否则起一个临时的
if "${REDIS_CLI:-redis-cli}" -p "$PORT" ping >/dev/null 2>&1; then
	echo "复用 127.0.0.1:$PORT 上已有的 redis"
else
	command -v "$REDIS_SERVER" >/dev/null 2>&1 || {
		echo "缺少 redis-server，且 $PORT 上没有可用实例"
		echo "（art:// 不行：那是进程内 kvspace，外部执行器看不见）"; exit 1
	}
	"$REDIS_SERVER" --port "$PORT" --save '' --appendonly no --daemonize yes \
		--pidfile "$TMP/r.pid" --logfile "$TMP/r.log"
	STARTED_REDIS=1
	sleep 1
fi

[ -x "$ROOT/kvlang" ] || (cd "$ROOT" && go build -o kvlang ./cmd/kvlang/)
"$KVSPACE_CLI" --kvspace "$DSN" clear >/dev/null 2>&1 || true

export KVLANG_KVSPACE="$DSN" KVSPACE_CLI="$KVSPACE_CLI"
python3 "$DIR/fake_backend.py" > "$TMP/be.log" 2>&1 &
echo $! > "$TMP/be.pid"

# 轮询等注册键出现，别用固定 sleep —— 后端每次注册都要 spawn 一个 Go 二进制，
# 机器一忙就超过任何拍脑袋的等待时间，在 CI 上表现为间歇性失败。
i=0
while [ "$i" -lt 100 ]; do
	case $("$KVSPACE_CLI" --kvspace "$DSN" get /sys/op/fake/0 2>/dev/null) in
		*running*) break ;;
	esac
	i=$((i + 1)); sleep 0.1
done
[ "$i" -lt 100 ] || { echo "FAIL: 后端 10 秒内未完成注册"; cat "$TMP/be.log"; exit 1; }

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
