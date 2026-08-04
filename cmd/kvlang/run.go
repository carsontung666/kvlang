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
	debug := fs.Bool("debug", false, "单步调试模式（仅供 ART 同进程控制器使用）")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: kvlang run [--debug] [-c code | {lib}.{func} | <file.kv|dir>]")
		fmt.Fprintln(os.Stderr, "  ART 不跨进程共享：独立执行 {lib}.{func} 需由同一进程预先装载；--debug 不能由外部 CLI 控制")
		fs.PrintDefaults()
	}
	fs.Parse(args)

	switch {
	case *code != "":
		runCode("inline", strings.NewReader(*code), *dsn, *debug)
	case fs.NArg() > 0:
		arg := fs.Arg(0)
		if isKVSourcePath(arg) {
			runFiles(*dsn, fs.Args(), *debug)
		} else if strings.Contains(arg, keytree.MemberSep) {
			parts := strings.SplitN(arg, keytree.MemberSep, 2)
			runLib(*dsn, parts[0], parts[1], *debug)
		} else {
			runLib(*dsn, arg, "init", *debug)
		}
	case !isTerminal():
		runCode("stdin", os.Stdin, *dsn, *debug)
	default:
		runLib(*dsn, "", "init", false)
	}
}

func isKVSourcePath(path string) bool {
	if strings.HasSuffix(path, ".kv") {
		return true
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// runLib 执行 /lib/{lib}.{func}。lib/func 为空时默认 "init"。
func runLib(dsn, lib, fn string, debug bool) {
	if fn == "" { fn = "init" }
	name := lib + keytree.MemberSep + fn
	if lib == "" { name = fn }
	kv := kvspace.Conn(dsn)
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
	cpu.Execute(firstPC)
	reportRunError(kv, vtid)
}

func reportRunError(kv kvspace.KVSpace, vtid string) {
	msgVal := kvspace.GetOne(kv, keytree.VThreadStatusMsg(vtid, "error"))
	if !kvspace.IsNone(msgVal) {
		pcVal := kvspace.GetOne(kv, keytree.VThreadPC(vtid))
		logx.Error("%s at %s", msgVal.String(), pcVal.String())
		os.Exit(1)
	}
}
