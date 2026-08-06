package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"kvlang/keytree"
	"kvlang/kvcpu"
	"github.com/array2d/kvspace-go"
	"kvlang/layout"
	"kvlang/rwir/builtin"
	"kvlang/logx"
	"kvlang/vthread"
)

// cmdRun 解析参数并路由：内联 / {lib}.{func} / 文件 / 管道。
func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	dsn   := fs.String("kvspace", defaultKVSpace(), kvspaceFlagDesc)
	code  := fs.String("c", "", "内联代码（直接执行字符串）")
	debug := fs.Bool("debug", false, "单步调试模式（交互式，每条指令暂停）")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: kvlang run [--debug] [-c code | {lib}.{func} | <file.kv|dir>]")
		fs.PrintDefaults()
	}
	fs.Parse(args)

	switch {
	case *code != "":
		runCode("inline", strings.NewReader(*code), *dsn, *debug)
	case fs.NArg() > 0:
		arg := fs.Arg(0)
		if !strings.HasSuffix(arg, ".kv") && strings.Contains(arg, keytree.MemberSep) {
			parts := strings.SplitN(arg, keytree.MemberSep, 2)
			runLib(parts[0], parts[1], *debug)
		} else if !strings.HasSuffix(arg, ".kv") {
			runLib(arg, "init", *debug)
		} else {
			runFiles(*dsn, fs.Args(), *debug)
		}
	case !isTerminal():
		runCode("stdin", os.Stdin, *dsn, *debug)
	default:
		runLib("", "init", false)
	}
}

// runLib 执行 /lib/{lib}.{func}。lib/func 为空时默认 "init"。
func runLib(lib, fn string, debug bool) {
	if fn == "" { fn = "init" }
	name := lib + keytree.MemberSep + fn
	if lib == "" { name = fn }
	kv := kvspace.Conn(defaultKVSpace())
	defer kv.DisConn()
	registerDefaultTerm(kv)
	executeEntry(kv, name, debug)
}

// executeEntry 创建 vthread 并同步执行。
func executeEntry(kv kvspace.KVSpace, entryName string, debug bool) {
	ctx := context.Background()
	vtid := vthread.AllocVtid(kv)
	kv.DelTree(keytree.VThread(vtid))
	kvspace.MkIndexRecursive(kv, keytree.VThread(vtid)+"/")
	builtin.WriteSysRwir(kv)
	firstPC := layout.Bootstrap(ctx, kv, vtid, entryName, nil)
	if firstPC == "" {
		logx.Fatal("[single] Bootstrap %s failed", entryName)
	}
	vthread.Set(ctx, kv, vtid, firstPC, "init")
	kv.Set([]kvspace.KVPair{
		{keytree.VThreadCtime(vtid), kvspace.NewTime(time.Now().UnixNano())},
		{keytree.VThreadTerm(vtid), kvspace.NewChar("kvlangrun")},
	})

	if debug {
		kv.Set([]kvspace.KVPair{{keytree.VThreadDebugger(vtid), kvspace.NewChar("break")}})
		logx.Info("[single] debug mode: executing %s", firstPC)
		cpu := kvcpu.New(kv, "single")
		cpu.Execute(firstPC)
		logx.Info("[dbg] execution finished")
		return
	}

	logx.Info("[single] executing %s", firstPC)
	cpu := kvcpu.New(kv, "single")
	err := cpu.Execute(firstPC)
	reportRunError(kv, vtid) // 有 ‥error/msg 时打印并 exit 1
	if err != nil {
		// Execute 报错、但 ‥error/msg 没写上。最典型的情形是 kvspace 自身出问题：
		// vthread.SetError 也要靠 kv.Set 才能写，写不进去就没有 msg 可读。
		// 丢掉这个返回值会让"执行失败"表现为 exit 0 —— 静默失败是最难查的一类。
		logx.Error("%v", err)
		os.Exit(1)
	}
}

func reportRunError(kv kvspace.KVSpace, vtid string) {
	msgVal := kvspace.GetOne(kv, keytree.VThreadStatusMsg(vtid, "error"))
	if !kvspace.IsNone(msgVal) {
		pcVal := kvspace.GetOne(kv, keytree.VThreadPC(vtid))
		logx.Error("%s at %s", msgVal.String(), pcVal.String())
		os.Exit(1)
	}
}
