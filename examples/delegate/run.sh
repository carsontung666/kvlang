#!/bin/sh
# 委托 rwir 的跨进程验收：起临时 redis → 起外部执行器 → 跑 echo.kv → 比对输出 → 全部清理。
#
# 为什么需要它：go test 里的执行器是同进程 goroutine（art:// 是进程内 kvspace），
# 它验证的是 VM 侧协议。而 examples/ 这套是用户真会跑的东西 —— CLI 的值编码、
# 参数顺序、键布局 —— 只有真起两个进程才验得到。协议改过而这里没跑，就会悄悄烂掉。
#
#   sh examples/delegate/run.sh
#
# 需要 redis-server 与 kvspace CLI 在 PATH 上，或用环境变量指定：
#   REDIS_SERVER=/path/to/redis-server KVSPACE_CLI=/path/to/kvspace sh examples/delegate/run.sh
#
# 端口策略（在共享机器上很要紧）：
#   不设 PORT  → 自己挑一个空闲高端口并**只连自己起的 redis**。端口被占就换一个。
#   显式设 PORT → 复用该端口上已有的 redis（CI 用 service 容器时走这条），
#                 此时脚本会 clear 整个 kvspace，所以只在你确知那是自己的实例时才设。
set -e

REDIS_SERVER="${REDIS_SERVER:-redis-server}"
KVSPACE_CLI="${KVSPACE_CLI:-kvspace}"
DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT=$(CDPATH= cd -- "$DIR/../.." && pwd)
TMP=$(mktemp -d)

STARTED_REDIS=""
PORT_GIVEN="${PORT:-}"

cleanup() {
	# 先杀后端并**等它真的退出**：它阻塞在 kvspace watch 子进程上，只 kill 不等的话
	# 那个子进程会以孤儿身份继续持有 redis 连接。
	if [ -f "$TMP/be.pid" ]; then
		BE=$(cat "$TMP/be.pid")
		# 顺序要紧：先杀它的 kvspace watch 子进程，再杀它本身。
		# 反过来的话子进程会被 reparent 成孤儿，而 go-redis 客户端会自动重连 ——
		# 一个"已经杀掉"的后端能在下一次运行时重新连上同一端口并抢走任务。
		pkill -P "$BE" 2>/dev/null || true
		kill "$BE" 2>/dev/null || true
		wait "$BE" 2>/dev/null || true
	fi
	# 只关自己起的那个 redis，且用 pidfile 而不是 redis-cli shutdown ——
	# 见下面 port_busy 的说明，本脚本不依赖 redis-cli。
	# 关完要等它真的退出：SIGTERM 后 redis 还要走一段收尾，脚本若立刻返回，
	# 在共享机器上就会短暂留下一个别人能看见的常驻进程。
	if [ -n "$STARTED_REDIS" ] && [ -f "$TMP/r.pid" ]; then
		RPID=$(cat "$TMP/r.pid")
		kill "$RPID" 2>/dev/null || true
		i=0
		while [ "$i" -lt 50 ] && kill -0 "$RPID" 2>/dev/null; do
			i=$((i + 1)); sleep 0.1
		done
		kill -0 "$RPID" 2>/dev/null && kill -9 "$RPID" 2>/dev/null || true
	fi
	rm -rf "$TMP"
}
trap cleanup EXIT INT TERM

command -v "$KVSPACE_CLI" >/dev/null 2>&1 || {
	echo "缺少 kvspace CLI。注意要从 go.mod 钉住的版本构建，不能用 @latest —"
	echo "唯一的 tag v0.1.0 与本仓库 API 及值编码都不同源："
	echo "  go build -o kvspace github.com/array2d/kvspace-go/cmd/kvspace"
	exit 1
}

# 探活用 kvspace CLI 而不是 redis-cli。
#
# 本脚本本来就硬依赖 kvspace CLI（上面已 command -v 检查过），再拉一个
# redis-cli 依赖会在最需要它的地方掉链子：GitHub runner 上 redis 是 service
# 容器，redis-cli 在容器里而不在 runner 上，ubuntu-latest 也不预装 redis-tools。
# 那样 port_busy 会恒假 —— 明明有一个活着的 redis，脚本却报「该端口上没有可用
# 的 redis」，把「缺少二进制」误诊成「服务不在」。
# 实测 kvspace CLI 探活：无服务时 73ms 内 exit 1，有服务时 exit 0。
port_busy() { "$KVSPACE_CLI" --kvspace "redis://127.0.0.1:$1" list / >/dev/null 2>&1; }

if [ -n "$PORT_GIVEN" ]; then
	# 显式指定端口：复用调用方提供的实例（CI 的 service 容器）
	PORT="$PORT_GIVEN"
	port_busy "$PORT" || {
		echo "FAIL: 指定了 PORT=$PORT 但该端口上没有可用的 redis"; exit 1
	}
	echo "复用 127.0.0.1:$PORT 上已有的 redis"
else
	command -v "$REDIS_SERVER" >/dev/null 2>&1 || {
		echo "缺少 redis-server（art:// 不行：那是进程内 kvspace，外部执行器看不见）"
		exit 1
	}
	# 自己挑端口：只用没人监听的，绝不连别人的实例 —— 下面会 clear 整个 kvspace。
	PORT=""
	i=0
	while [ "$i" -lt 20 ]; do
		CAND=$(awk -v s="$$" -v k="$i" 'BEGIN{srand(s+k*7919);print 40000+int(rand()*20000)}')
		if ! port_busy "$CAND"; then PORT="$CAND"; break; fi
		i=$((i + 1))
	done
	[ -n "$PORT" ] || { echo "FAIL: 20 次都没挑到空闲端口"; exit 1; }
	"$REDIS_SERVER" --port "$PORT" --bind 127.0.0.1 --save '' --appendonly no \
		--daemonize yes --pidfile "$TMP/r.pid" --logfile "$TMP/r.log"
	STARTED_REDIS=1
	i=0
	while [ "$i" -lt 100 ]; do
		port_busy "$PORT" && break
		i=$((i + 1)); sleep 0.1
	done
	port_busy "$PORT" || { echo "FAIL: redis 10 秒内没起来"; cat "$TMP/r.log"; exit 1; }
	echo "已在 127.0.0.1:$PORT 起临时 redis"
fi

DSN="redis://127.0.0.1:$PORT"
# 二进制建到 $TMP，不往仓库根写文件（那里的 kvlang 恰好被 .gitignore 挡着，
# 于是"污染了工作区"这件事看不出来）
KVLANG_BIN="$ROOT/kvlang"
[ -x "$KVLANG_BIN" ] || { KVLANG_BIN="$TMP/kvlang"; (cd "$ROOT" && go build -o "$KVLANG_BIN" ./cmd/kvlang/); }
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

# stdout / stderr 分开收。合并的话任何一行 warn/log 都会变成一处看不懂的 diff
# 失败 —— 本改动给 WriteRwir/WriteFunc 加的 logx.Warn 就是这种行。
# 判定与 tutorial/test.py 对齐：比对 stdout，stderr 只在失败时打印出来看。
"$KVLANG_BIN" "$DIR/echo.kv" > "$TMP/out.txt" 2> "$TMP/err.txt" || {
	echo "FAIL: kvlang 退出非零"; cat "$TMP/out.txt"; cat "$TMP/err.txt"; cat "$TMP/be.log"; exit 1
}

# 期望输出取自 echo.kv 头部的 `#   ` 注释块，与 tutorial/test.py 同一注释格式
sed -n '/^# 期望输出:/,/^#$/p' "$DIR/echo.kv" | sed -n 's/^#   //p' > "$TMP/want.txt"
if diff -u "$TMP/want.txt" "$TMP/out.txt" > "$TMP/diff.txt"; then
	echo "PASS: 跨进程委托输出与期望一致"
	cat "$TMP/out.txt"
	[ -s "$TMP/err.txt" ] && { echo "--- stderr（非致命，仅供参考）---"; cat "$TMP/err.txt"; }
	exit 0
else
	echo "FAIL: stdout 不符"; cat "$TMP/diff.txt"
	[ -s "$TMP/err.txt" ] && { echo "--- kvlang stderr ---"; cat "$TMP/err.txt"; }
	echo "--- 后端日志 ---"; cat "$TMP/be.log"; exit 1
fi
