// Package logx 提供编译器风格级别日志，通过 LOG_LEVEL 环境变量控制。
//
//	LOG_LEVEL=debug  输出所有级别
//	LOG_LEVEL=info   输出 info/warn/error
//	LOG_LEVEL=warn   输出 warn/error（默认）
//	LOG_LEVEL=error  仅输出 error
//
// 输出格式对齐五大语言编译器诊断（GCC/Go/Rust/Python/V8）：
// 无时间戳、无 key=value 结构，纯文本直接输出到 stderr。
//
// 范式：
//
//	Debug/Info — 操作消息，不加前缀
//	Warn/Error  — 自动加 "warn: " / "error: " 前缀
//	Fatal       — 同 Error + os.Exit(1)
//	Diag        — 打印 parser.Diagnostic，格式由 Diagnostic.String() 定义
package logx

import (
	"fmt"
	"io"
	"os"
	"strings"

	"kvlang/parser"
)

// out 是所有诊断的落点。默认 stderr；测试用 SetOutputForTest 换掉它，
// 好让"某某情况必须出声告警"这类断言能落在真实输出上，而不是内部标志位。
var out io.Writer = os.Stderr

// SetOutputForTest 临时改写日志落点，返回还原函数。仅供测试使用。
func SetOutputForTest(w io.Writer) func() {
	old := out
	out = w
	return func() { out = old }
}

type level int

const (
	levelDebug level = iota
	levelInfo
	levelWarn
	levelError
)

var currentLevel = levelWarn

func init() {
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug":
		currentLevel = levelDebug
	case "info":
		currentLevel = levelInfo
	case "warn", "":
		currentLevel = levelWarn
	case "error":
		currentLevel = levelError
	}
}

func Debug(format string, args ...any) {
	if currentLevel <= levelDebug {
		fmt.Fprintf(out, format+"\n", args...)
	}
}

func Info(format string, args ...any) {
	if currentLevel <= levelInfo {
		fmt.Fprintf(out, format+"\n", args...)
	}
}

func Warn(format string, args ...any) {
	if currentLevel <= levelWarn {
		fmt.Fprintf(out, "warn: "+format+"\n", args...)
	}
}

func Error(format string, args ...any) {
	if currentLevel <= levelError {
		fmt.Fprintf(out, "error: "+format+"\n", args...)
	}
}

func Fatal(format string, args ...any) {
	Error(format, args...)
	os.Exit(1)
}

// Diag 打印一条 parser 诊断，格式由 Diagnostic.String() 定义（已含 level 前缀）。
func Diag(d parser.Diagnostic) {
	if d.Info {
		if currentLevel <= levelInfo {
			fmt.Fprintln(out, d.String())
		}
	} else if d.Warn {
		if currentLevel <= levelWarn {
			fmt.Fprintln(out, d.String())
		}
	} else {
		if currentLevel <= levelError {
			fmt.Fprintln(out, d.String())
		}
	}
}

// DiagWithSource 打印一条带源码上下文和 ^ 指示符的诊断（对标 GCC 多行风格）。
func DiagWithSource(d parser.Diagnostic) {
	if d.Info && currentLevel > levelInfo {
		return
	}
	if !d.Info && d.Warn && currentLevel > levelWarn {
		return
	}
	if !d.Info && !d.Warn && currentLevel > levelError {
		return
	}
	if d.Source != "" {
		fmt.Fprintf(out, "%s\n  %s\n  %s%c\n", d.String(), d.Source,
			strings.Repeat(" ", d.Pos.Col-1), '^')
	} else {
		fmt.Fprintln(out, d.String())
	}
}
