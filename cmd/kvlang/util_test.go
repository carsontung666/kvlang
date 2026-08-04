package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/array2d/kvspace-go"
	"github.com/array2d/kvspace-go/art"
)

func TestDefaultKVSpaceUsesART(t *testing.T) {
	t.Setenv("KVLANG_KVSPACE", "")
	if got := defaultKVSpace(); got != "art://local" {
		t.Fatalf("defaultKVSpace() = %q, want art://local", got)
	}
}

func TestDefaultKVSpaceHonorsEnvironment(t *testing.T) {
	t.Setenv("KVLANG_KVSPACE", "art://test")
	if got := defaultKVSpace(); got != "art://test" {
		t.Fatalf("defaultKVSpace() = %q, want art://test", got)
	}
}

func TestIsKVSourcePath(t *testing.T) {
	if !isKVSourcePath("example.kv") {
		t.Fatal(".kv file should be treated as a source path")
	}
	if !isKVSourcePath(t.TempDir()) {
		t.Fatal("directory should be treated as a source path")
	}
	if isKVSourcePath("math.sum") {
		t.Fatal("library entry should not be treated as a source path")
	}
}

func TestRunUsesExplicitKVSpaceThroughExecution(t *testing.T) {
	explicitName := testStoreName(t, "run-explicit")
	fallbackName := testStoreName(t, "run-fallback")
	defer destroyTestStores(t, explicitName, fallbackName)

	explicitProgram := writeTestProgram(t, "explicit.kv", `
lib selected {
	rwfunc init() -> () { 2 -> /run_explicit }
}`)
	fallbackProgram := writeTestProgram(t, "fallback.kv", `
lib selected {
	rwfunc init() -> () { 1 -> /run_fallback }
}`)
	layoutTestProgram(t, explicitName, explicitProgram)
	layoutTestProgram(t, fallbackName, fallbackProgram)
	t.Setenv("KVLANG_KVSPACE", "art://"+fallbackName)

	cmdRun([]string{"--kvspace", "art://" + explicitName, "selected"})

	assertTestValue(t, explicitName, "/run_explicit", "2")
	assertTestMissing(t, fallbackName, "/run_fallback")
}

func TestLayoutAndRunUsesOneExplicitKVSpace(t *testing.T) {
	explicitName := testStoreName(t, "layout-explicit")
	fallbackName := testStoreName(t, "layout-fallback")
	defer destroyTestStores(t, explicitName, fallbackName)

	explicitProgram := writeTestProgram(t, "explicit.kv", `
rwfunc main() -> () { 2 -> /layout_explicit }
main()`)
	fallbackProgram := writeTestProgram(t, "fallback.kv", `
rwfunc main() -> () { 1 -> /layout_fallback }
main()`)
	layoutTestProgram(t, fallbackName, fallbackProgram)
	t.Setenv("KVLANG_KVSPACE", "art://"+fallbackName)

	cmdLayoutAndRun([]string{"--kvspace", "art://" + explicitName, explicitProgram})

	assertTestValue(t, explicitName, "/layout_explicit", "2")
	assertTestMissing(t, fallbackName, "/layout_fallback")
}

func testStoreName(t *testing.T, suffix string) string {
	t.Helper()
	return strings.NewReplacer("/", "-", " ", "-").Replace(t.Name()) + "-" + suffix
}

func writeTestProgram(t *testing.T, name, source string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func layoutTestProgram(t *testing.T, storeName, path string) {
	t.Helper()
	kv := art.Conn(storeName)
	defer kv.DisConn()
	layoutFiles(kv, []string{path})
}

func assertTestValue(t *testing.T, storeName, key, want string) {
	t.Helper()
	kv := art.Conn(storeName)
	defer kv.DisConn()
	if got := kvspace.GetOne(kv, key).String(); got != want {
		t.Fatalf("%s in %s = %q, want %q", key, storeName, got, want)
	}
}

func assertTestMissing(t *testing.T, storeName, key string) {
	t.Helper()
	kv := art.Conn(storeName)
	defer kv.DisConn()
	if got := kvspace.GetOne(kv, key); !kvspace.IsNone(got) {
		t.Fatalf("%s unexpectedly exists in %s: %s", key, storeName, got.String())
	}
}

func destroyTestStores(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if err := art.Destroy(name); err != nil {
			t.Errorf("destroy ART store %s: %v", name, err)
		}
	}
}
