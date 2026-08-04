package main

import (
	"os"
)

// defaultKVSpace 返回 kvspace DSN 默认值：KVLANG_KVSPACE 环境变量覆盖，否则使用进程内 ART。
func defaultKVSpace() string {
	if v := os.Getenv("KVLANG_KVSPACE"); v != "" {
		return v
	}
	return "art://local"
}

const kvspaceFlagDesc = "kvspace 地址（DSN，如 art://local；默认可由 KVLANG_KVSPACE 覆盖）"
