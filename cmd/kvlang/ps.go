package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/array2d/kvspace-go"
	"kvlang/keytree"
	"kvlang/vthread"
)

func cmdPS(args []string) {
	fs := flag.NewFlagSet("ps", flag.ExitOnError)
	dsn := fs.String("kvspace", defaultKVSpace(), kvspaceFlagDesc)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: kvlang ps [--kvspace <dsn>]")
		fmt.Fprintln(os.Stderr, "  ART 仅能列出当前进程的 vthread；独立 CLI 进程通常为空")
		fs.PrintDefaults()
	}
	fs.Parse(args)

	kv := kvspace.Conn(*dsn)
	defer kv.DisConn()

	vtids := kv.List(keytree.VthreadRoot+keytree.PathSegSep, false)

	var ids []int64
	for _, v := range vtids {
		v = strings.TrimSuffix(v, keytree.PathSegSep)
		if id, e := strconv.ParseInt(v, 10, 64); e == nil {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	fmt.Printf("%-20s %-8s %s\n", "PID", "STATUS", "PC")
	for _, id := range ids {
		vtid := strconv.FormatInt(id, 10)
		pc, status := vthread.Get(context.Background(), kv, vtid)
		if status == "" {
			status = "-"
		}
		fmt.Printf("%-20s %-8s %s\n", vtid, status, pc)
	}
}
