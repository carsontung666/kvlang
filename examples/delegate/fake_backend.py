#!/usr/bin/env python3
"""委托机制的最小外部执行器。

它做的事就是委托 ABI 的全部内容：
  1. 在 /sys/op/fake/ 注册能力与实例
  2. watch 命令队列
  3. 收到任务 → 把 input 原样写进 output key
  4. 置 .status=done，再 notify per-task 的 .done 键

跑法（两个终端）：
    redis-server --port 6379 &
    export KVLANG_KVSPACE=redis://127.0.0.1:6379
    python3 examples/delegate/fake_backend.py &
    ./kvlang examples/delegate/echo.kv

art:// 下不工作 —— 那是进程内 kvspace，外部进程看不见 /sys/op。

关停执行器时要连它的 kvspace 子进程一起杀（先 pkill -P 再 kill 父进程）。
只杀父进程的话，阻塞在 watch 上的子进程会变成孤儿，而 go-redis 客户端会
自动重连 —— 一个"已经停掉"的后端能重新连上并抢走后续任务。这不是本脚本
特有的问题，任何用子进程做 watch 的执行器实现都要注意。
"""

import json
import os
import subprocess
import sys

KV = os.environ.get("KVSPACE_CLI", "kvspace")
DSN = os.environ.get("KVLANG_KVSPACE", "redis://127.0.0.1:6379")

BACKEND = "fake"
INSTANCE = "0"
OPS = ["echo"]                      # 注册的是剥掉命名空间前缀的名字：fake.echo → echo

# watch 用有限超时而不是 0（永久阻塞）：命令队列是持久的（redis 是 LPUSH/BLPOP
# 列表），两次 watch 之间推进来的任务不会丢，而有限超时能让 kvspace 子进程定期
# 退出，把"父进程被杀、子进程成孤儿"的窗口压到一个超时周期以内。
#
# 必须是 Go duration 字面量（"2s"），裸数字会被 flag 包拒绝：
#   invalid value "2" for flag -timeout: parse error
WATCH_TIMEOUT = "2s"


def kv(*args, timeout=None):
    """调 kvspace CLI，返回 stdout。

    CLI 调用失败（用错 flag、redis 掉线）会返回空 stdout，与"watch 超时"
    无法区分。主循环因此会立刻重试 —— 必须把失败计出来，否则一个用错的
    flag 就变成刷屏的热循环。返回 (stdout, ok)。
    """
    cmd = [KV, "--kvspace", DSN] + list(args)
    r = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
    if r.returncode != 0 and r.stderr.strip():
        print(f"[backend] {' '.join(args[:2])}: {r.stderr.strip()}", file=sys.stderr, flush=True)
    return r.stdout.strip(), r.returncode == 0


def kvset(key, value):
    """写一个字符串值。

    必须显式带 kind 前缀：kvspace CLI 的 ParseValue 按**第一个冒号**切分，
    把前缀当 kind 标签。裸传 '{"status": ...}' 会被解析成 kind='{"status"' 而报
    unknown kind；裸传任何含冒号的值（时间、URL、"a:b"）同样中招。
    """
    out, _ = kv("set", key, "string:" + value)
    return out


def register():
    """能力声明 + 实例记录。

    /sys/op/<b>/func/<op> 只要存在即算支持，值无所谓。
    /sys/op/<b>/<n> 是 {status, load} JSON，Select 只挑 status=running 的。
    """
    for op in OPS:
        kvset(f"/sys/op/{BACKEND}/func/{op}", "1")
    kvset(f"/sys/op/{BACKEND}/{INSTANCE}",
          json.dumps({"status": "running", "load": 0.0}))
    print(f"[backend] registered /sys/op/{BACKEND} ops={OPS}", flush=True)


def handle(task):
    # 字段缺失就跳过这一条，别让一个畸形任务打死执行器 —— VM 那侧要等满
    # 一个超时周期才发现，而后续任务全都收不到。
    missing = [k for k in ("id", "done_key") if not task.get(k)]
    if missing:
        print(f"[backend] 任务缺字段 {missing}，跳过: {task!r}", file=sys.stderr, flush=True)
        return
    tid = task["id"]
    inputs = [i.get("value", "") for i in task.get("inputs") or []]
    outputs = [o["key"] for o in (task.get("outputs") or []) if o.get("key")]
    status_key = f"/sys/task/{tid}.status"
    done_key = task["done_key"]        # 协议自带，不要自己拼 —— 键布局只该有一个来源
    print(f"[backend] task={tid} op={task['opcode']} in={inputs} out={outputs}", flush=True)

    # echo：原样回写第一个输入
    result = inputs[0] if inputs else ""
    for key in outputs:
        kvset(key, result)

    # 顺序要紧：先写 outputs，再落持久状态，最后才发信号。
    # VM 那侧 Watch 返回后会复查 .status，信号丢了也不会误判成功；反过来
    # 先发信号后写值，VM 可能在值落地前就继续执行。
    kvset(status_key, "done")
    # notify 的载荷**不**走 ParseValue：CLI 直接 kvspace.NewChar(argv[3])，
    # 所以这里不能像 kvset 那样加 "string:" 前缀，否则收到的是字面量 "string:1"。
    kv("notify", done_key, "1")


def main():
    register()
    queue = f"/sys/op/{BACKEND}/{INSTANCE}/cmd"
    print(f"[backend] watching {queue}", flush=True)
    fails = 0
    while True:
        # --timeout 必须在位置参数**之前**：Go 的 flag 包遇到第一个非 flag 实参就
        # 停止解析，写在 queue 后面会被当成尾随操作数直接忽略。
        raw, ok = kv("watch", "--timeout", WATCH_TIMEOUT, queue)
        if not ok:
            fails += 1
            if fails >= 5:
                print("[backend] watch 连续失败 5 次，退出", file=sys.stderr, flush=True)
                return 1
            continue
        fails = 0
        if not raw:
            continue
        try:
            task = json.loads(raw)
        except json.JSONDecodeError as e:
            # 若这里报 "invalid character '\x00'"，说明 VM 侧用了 NewBytes 而非 NewChar
            # —— NewBytes 会给每个元素追加一个 NUL。
            print(f"[backend] bad task: {e}: {raw!r}", file=sys.stderr, flush=True)
            continue
        if not isinstance(task, dict):
            print(f"[backend] 任务不是对象，跳过: {task!r}", file=sys.stderr, flush=True)
            continue
        try:
            handle(task)
        except Exception as e:            # 一条任务的问题不该拖垮执行器
            print(f"[backend] handle 失败: {e!r}", file=sys.stderr, flush=True)


if __name__ == "__main__":
    try:
        sys.exit(main() or 0)
    except KeyboardInterrupt:
        pass
