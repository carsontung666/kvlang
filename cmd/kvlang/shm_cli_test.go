//go:build linux && cgo

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/array2d/kvspace-go"
	"kvlang/internal/kvshm"
	"kvlang/keytree"
	"kvlang/vthread"
)

func TestSharedMemoryCLIWorkflows(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "kvlang")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build kvlang: %v\n%s", err, output)
	}

	layoutProgram := filepath.Join(t.TempDir(), "layout.kv")
	if err := os.WriteFile(layoutProgram, []byte(`
lib selected {
	rwfunc init() -> () { 31 -> /cli_layout }
}
`), 0600); err != nil {
		t.Fatal(err)
	}
	combinedProgram := filepath.Join(t.TempDir(), "combined.kv")
	if err := os.WriteFile(combinedProgram, []byte(`
rwfunc main() -> () { 47 -> /cli_combined }
main()
`), 0600); err != nil {
		t.Fatal(err)
	}

	for index, scheme := range []string{
		"shm-art-bump",
		"shm-art-box",
		"shm-hash-box",
		"shm-trie-box",
	} {
		scheme := scheme
		t.Run(scheme, func(t *testing.T) {
			layoutName := fmt.Sprintf("kvlang-cli-layout-%d-%d", os.Getpid(), index)
			combinedName := fmt.Sprintf("kvlang-cli-combined-%d-%d", os.Getpid(), index)
			psID := fmt.Sprintf("90000000000000%d", index)
			psStatus := "dsn-" + scheme
			psPC := "/ps/" + scheme
			t.Cleanup(func() {
				_ = kvshm.Destroy(layoutName)
				_ = kvshm.Destroy(combinedName)
			})

			layoutDSN := scheme + "://" + layoutName
			runCLI(t, binary, "layout", "--kvspace", layoutDSN, layoutProgram)
			runCLI(t, binary, "run", "--kvspace", layoutDSN, "selected")
			store := kvspace.Conn(layoutDSN)
			if got := kvspace.GetOne(store, "/cli_layout").String(); got != "31" {
				_ = store.DisConn()
				t.Fatalf("layout+run value=%q", got)
			}
			kvspace.MkIndexRecursive(store, keytree.VThread(psID)+"/")
			vthread.Set(context.Background(), store, psID, psPC, psStatus)
			if err := store.DisConn(); err != nil {
				t.Fatal(err)
			}
			psOutput := runCLI(t, binary, "ps", "--kvspace", layoutDSN)
			for _, field := range []string{psID, psStatus, psPC} {
				if !strings.Contains(psOutput, field) {
					t.Fatalf("ps output=%q does not contain DSN sentinel %q", psOutput, field)
				}
			}

			combinedDSN := scheme + "://" + combinedName
			runCLI(
				t,
				binary,
				"layoutandrun",
				"--kvspace",
				combinedDSN,
				combinedProgram,
			)
			combined := kvspace.Conn(combinedDSN)
			if got := kvspace.GetOne(combined, "/cli_combined").String(); got != "47" {
				_ = combined.DisConn()
				t.Fatalf("layoutandrun value=%q", got)
			}
			if err := combined.DisConn(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func runCLI(t *testing.T, binary string, args ...string) string {
	t.Helper()
	command := exec.Command(binary, args...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", binary, strings.Join(args, " "), err, output)
	}
	return string(output)
}
