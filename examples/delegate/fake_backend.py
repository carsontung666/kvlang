#!/usr/bin/env python3
"""委托机制的最小外部执行器 —— 阶段 1 验收用。

它做的事就是委托 ABI 的全部内容：
  1. 在 /sys/op/fake/ 注册能力与实例
  2. watch 命令队列
  3. 收到任务 → 把 input 原样写进 output key
  4. 置 .status=done，再 notify per-task 的 .done 键

跑法（两个终端）：
    redis-server &
    export KVLANG_KVSPACE=redis://127.0.0.1:6379
    python3 examples/delegate/fake_backend.py &
    ./kvlang examples/delegate/echo.kv

注意 art:// 下不工作 —— 那是进程内 kvspace，外部进程看不见 /sys/op。
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


def kv(*args, timeout=None):
    """调 kvspace CLI，返回 stdout。"""
    cmd = [KV, "--kvspace", DSN] + list(args)
    r = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
    if r.returncode != 0 and r.stderr.strip():
        print(f"[backend] {' '.join(args[:2])}: {r.stderr.strip()}", file=sys.stderr)
    return r.stdout.strip()


def kvset(key, value):
    """写一个字符串值。

    必须显式带 kind 前缀：kvspace CLI 的 ParseValue 按**第一个冒号**切分，
    把前缀当 kind 标签。裸传 '{"status": ...}' 会被解析成 kind='{"status"' 而报
    unknown kind；裸传任何含冒号的值（时间、URL、"a:b"）同样中招。
    """
    return kv("set", key, "string:" + value)


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
    tid = task["id"]
    inputs = [i.get("value", "") for i in task["inputs"]]
    outputs = [o["key"] for o in task["outputs"]]
    status_key = f"/sys/task/{tid}.status"
    done_key = task["done_key"]        # 协议自带，不要自己拼 —— 键布局只该有一个来源
    print(f"[backend] task={tid} op={task['opcode']} in={inputs} out={outputs}", flush=True)

    kvset(status_key, "running")

    # echo：原样回写第一个输入
    result = inputs[0] if inputs else ""
    for key in outputs:
        kvset(key, result)

    # 顺序要紧：先落持久状态，再发信号。
    # VM 那侧 Watch 返回后会复查 .status，信号丢了也不会误判成功。
    kvset(status_key, "done")
    kv("notify", done_key, "string:1")


def main():
    register()
    queue = f"/sys/op/{BACKEND}/{INSTANCE}/cmd"
    print(f"[backend] watching {queue}", flush=True)
    while True:
        # --timeout 必须在位置参数**之前**：Go 的 flag 包遇到第一个非 flag 实参就
        # 停止解析，写在 queue 后面会被当成尾随操作数直接忽略。
        raw = kv("watch", "--timeout", "0", queue)
        if not raw:
            continue
        try:
            task = json.loads(raw)
        except json.JSONDecodeError as e:
            # 若这里报 "invalid character '\x00'"，说明 VM 侧用了 NewBytes 而非 NewChar
            # —— NewBytes 会给每个元素追加一个 NUL。
            print(f"[backend] bad task: {e}: {raw!r}", file=sys.stderr, flush=True)
            continue
        handle(task)


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        pass
