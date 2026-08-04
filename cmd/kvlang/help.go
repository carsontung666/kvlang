package main

import (
	"fmt"
	"os"
)

const helpText = `kvlang — KV language VM interpreter

usage:
  kvlang [--kvspace <dsn>] <file|dir>…      装载并执行
  kvlang layoutandrun <file|dir>…           装载并执行（显式子命令）
  kvlang -c "code"                          执行内联代码
  echo "code" | kvlang                      执行管道代码（stdin 非终端）

  kvlang vet [--dump] [--lower] [-c code | <file.kv>]  语法检查
  kvlang format [-w] [-c code | <file.kv>]  格式化（别名 fmt；默认打印，-w 原地写回）
  kvlang help                                显示此帮助

选项:
  --kvspace <dsn>                kvspace 地址（默认 art://local；KVLANG_KVSPACE 可覆盖）

说明:
  默认 ART 存储仅在当前进程内有效且不持久化；不同 kvlang/kvspace 进程不共享状态。
  需要装载后执行时，请在同一次调用中传入文件，或使用 layoutandrun。

示例:
  kvlang file.kv                           装载并执行
  kvlang layoutandrun lib.kv main.kv       多文件装载并执行
  kvlang -c 'x = 40 + 2; print(x)'      内联执行（= 等价于 <-）
  kvlang vet -c 'a = { k=1 }'           语法检查内联代码
`

func showHelp() {
	fmt.Fprint(os.Stderr, helpText)
	os.Exit(0)
}
