//go:build linux && cgo

package kvshm

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/array2d/kvspace-go"
)

var testNameSequence atomic.Uint64

var nativeSchemes = []struct {
	name   string
	engine engine
}{
	{"shm-art-bump", engineArtBump},
	{"shm-art-box", engineArtBox},
	{"shm-hash-box", engineHashBox},
	{"shm-trie-box", engineTrieBox},
}

func uniqueNativeName(label string) string {
	return fmt.Sprintf(
		"kvlang-%s-%d-%d",
		label,
		os.Getpid(),
		testNameSequence.Add(1),
	)
}

func connectNative(t *testing.T, scheme, name string) kvspace.KVSpace {
	t.Helper()
	store := kvspace.Conn(scheme + "://" + name)
	t.Cleanup(func() {
		_ = store.DisConn()
		_ = destroyNative(name)
	})
	return store
}

func getOne(store kvspace.KVSpace, prefix, key string) kvspace.XValue {
	return store.Get(prefix, []string{key})[0]
}

func TestNativeSchemesSemanticSurface(t *testing.T) {
	for _, candidate := range nativeSchemes {
		candidate := candidate
		t.Run(candidate.name, func(t *testing.T) {
			name := uniqueNativeName(candidate.name)
			store := connectNative(t, candidate.name, name)
			client, ok := store.(*nativeClient)
			if !ok {
				t.Fatalf("factory returned %T", store)
			}
			actualEngine, err := client.engineID()
			if err != nil {
				t.Fatal(err)
			}
			if actualEngine != candidate.engine {
				t.Fatalf("engine=%d, want %d", actualEngine, candidate.engine)
			}

			if err := store.Set([]kvspace.KVPair{
				{Key: "/a/x", Val: kvspace.NewInt64(42)},
				{Key: "/a/y", Val: kvspace.NewChar("hello")},
			}); err != nil {
				t.Fatal(err)
			}
			if got := getOne(store, "/a/", "x").String(); got != "42" {
				t.Fatalf("x=%q", got)
			}
			if got := store.List("/a/", false); !reflect.DeepEqual(got, []string{"x", "y"}) {
				t.Fatalf("list=%v", got)
			}

			second := kvspace.Conn(candidate.name + "://" + name)
			defer second.DisConn()
			if got := getOne(second, "/a/", "y").String(); got != "hello" {
				t.Fatalf("second client y=%q", got)
			}

			if err := store.Mkindex("/target/"); err != nil {
				t.Fatal(err)
			}
			if err := store.Link("/target/", "/alias/"); err != nil {
				t.Fatal(err)
			}
			if err := store.Set([]kvspace.KVPair{{
				Key: "/alias/value", Val: kvspace.NewBool(true),
			}}); err != nil {
				t.Fatal(err)
			}
			if got := getOne(store, "/target/", "value").String(); got != "true" {
				t.Fatalf("linked value=%q", got)
			}

			if err := store.Notify("/ready", kvspace.NewChar("done")); err != nil {
				t.Fatal(err)
			}
			if got := store.Watch("/ready", 100*time.Millisecond).String(); got != "done" {
				t.Fatalf("watch=%q", got)
			}
			if !kvspace.IsNone(store.Watch("/ready", time.Microsecond)) {
				t.Fatal("sub-millisecond timeout did not return None")
			}

			if err := store.DelTree("/a/"); err != nil {
				t.Fatal(err)
			}
			if !kvspace.IsNone(getOne(store, "/a/", "x")) {
				t.Fatal("DelTree left /a/x")
			}
			if candidate.engine == engineArtBump {
				if err := client.Compact(); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Clear(); err != nil {
				t.Fatal(err)
			}
			if got := store.List("/", false); len(got) != 0 {
				t.Fatalf("clear left root entries: %v", got)
			}
		})
	}
}

func TestNativeBatchValidationKeepsValidPrefix(t *testing.T) {
	name := uniqueNativeName("batch")
	store := connectNative(t, "shm-hash-box", name)
	err := store.Set([]kvspace.KVPair{
		{Key: "/kept", Val: kvspace.NewInt64(1)},
		{Key: "/bad\x00path", Val: kvspace.NewInt64(2)},
		{Key: "/never", Val: kvspace.NewInt64(3)},
	})
	if !errors.Is(err, kvspace.ErrInvalidPath) {
		t.Fatalf("Set error=%v", err)
	}
	if got := getOne(store, "/", "kept").String(); got != "1" {
		t.Fatalf("valid prefix was not committed: %q", got)
	}
	if !kvspace.IsNone(getOne(store, "/", "never")) {
		t.Fatal("pair after validation error was committed")
	}

	if err := store.Set([]kvspace.KVPair{{Key: "/other", Val: kvspace.NewInt64(4)}}); err != nil {
		t.Fatal(err)
	}
	err = store.Del("/kept", "/bad\x00path", "/other")
	if !errors.Is(err, kvspace.ErrInvalidPath) {
		t.Fatalf("Del error=%v", err)
	}
	if !kvspace.IsNone(getOne(store, "/", "kept")) {
		t.Fatal("valid Del prefix was not committed")
	}
	if got := getOne(store, "/", "other").String(); got != "4" {
		t.Fatalf("Del continued after invalid key: %q", got)
	}

	err = store.Set([]kvspace.KVPair{
		{Key: "/before-none", Val: kvspace.NewInt64(5)},
		{Key: "/none", Val: kvspace.None{}},
	})
	if err != nil {
		t.Fatalf("None Set error=%v", err)
	}
	if got := getOne(store, "/", "before-none").String(); got != "5" {
		t.Fatalf("prefix before None was not committed: %q", got)
	}
	if got := store.List("/", false); !reflect.DeepEqual(got, []string{"other", "before-none", "none"}) {
		t.Fatalf("None value was not indexed: %v", got)
	}

	err = store.Set([]kvspace.KVPair{{Key: "/bad-directory/", Val: kvspace.NewChar("not-index")}})
	if !errors.Is(err, kvspace.ErrInvalidDirValue) {
		t.Fatalf("directory value error=%v, want ErrInvalidDirValue", err)
	}
}

func TestNativeExtIndexRequiresDirectoryPaths(t *testing.T) {
	name := uniqueNativeName("extindex-paths")
	store := connectNative(t, "shm-hash-box", name)

	tests := []struct {
		name    string
		path    string
		extPath string
	}{
		{name: "path", path: "/mount", extPath: "/target/"},
		{name: "extension path", path: "/mount/", extPath: "/target"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := store.ExtIndex(test.path, test.extPath)
			if !errors.Is(err, kvspace.ErrDirMustEndWithSlash) {
				t.Fatalf("ExtIndex(%q, %q) error=%v, want ErrDirMustEndWithSlash", test.path, test.extPath, err)
			}
		})
	}
}

func TestNativeDisconnectCancelsWatch(t *testing.T) {
	name := uniqueNativeName("disconnect")
	store := connectNative(t, "shm-art-box", name)
	result := make(chan kvspace.XValue, 1)
	panicked := make(chan any, 1)
	go func() {
		defer func() { panicked <- recover() }()
		result <- store.Watch("/never", 0)
	}()
	time.Sleep(20 * time.Millisecond)
	if err := store.DisConn(); err != nil {
		t.Fatal(err)
	}
	select {
	case value := <-result:
		if !kvspace.IsNone(value) {
			t.Fatalf("cancelled Watch=%v", value)
		}
	case panicValue := <-panicked:
		if panicValue != nil {
			t.Fatalf("Watch panicked: %v", panicValue)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Watch was not cancelled by DisConn")
	}
	if err := store.Set([]kvspace.KVPair{{Key: "/closed", Val: kvspace.NewInt64(1)}}); !errors.Is(err, kvspace.ErrDisconnected) {
		t.Fatalf("Set after close error=%v", err)
	}
}

func TestNativeEngineMismatch(t *testing.T) {
	name := uniqueNativeName("mismatch")
	store := connectNative(t, "shm-art-bump", name)
	if err := store.Set([]kvspace.KVPair{{Key: "/kept", Val: kvspace.NewInt64(9)}}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("opening the same region with a different engine did not panic")
		}
		if got := getOne(store, "/", "kept").String(); got != "9" {
			t.Fatalf("mismatch damaged existing region: %q", got)
		}
	}()
	_ = kvspace.Conn("shm-trie-box://" + name)
}

func TestNativeCrossProcessAllSchemes(t *testing.T) {
	if os.Getenv("KVLANG_SHM_HELPER") == "1" {
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range nativeSchemes {
		candidate := candidate
		t.Run(candidate.name, func(t *testing.T) {
			name := uniqueNativeName("process")
			store := connectNative(t, candidate.name, name)
			command := exec.Command(
				executable,
				"-test.run=^TestNativeHelperProcess$",
				"--",
				candidate.name,
				name,
			)
			command.Env = append(os.Environ(), "KVLANG_SHM_HELPER=1")
			if output, runErr := command.CombinedOutput(); runErr != nil {
				t.Fatalf("helper: %v\n%s", runErr, output)
			}
			if got := getOne(store, "/child/", "value").String(); got != "73" {
				t.Fatalf("cross-process value=%q", got)
			}
		})
	}
}

func TestNativeHelperProcess(t *testing.T) {
	if os.Getenv("KVLANG_SHM_HELPER") != "1" {
		return
	}
	separator := -1
	for index, argument := range os.Args {
		if argument == "--" {
			separator = index
			break
		}
	}
	if separator < 0 || len(os.Args) != separator+3 {
		t.Fatalf("helper args: %q", os.Args)
	}
	scheme, name := os.Args[separator+1], os.Args[separator+2]
	store := kvspace.Conn(scheme + "://" + name)
	if err := store.Set([]kvspace.KVPair{{
		Key: "/child/value",
		Val: kvspace.NewInt64(73),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := store.DisConn(); err != nil {
		t.Fatal(err)
	}
}

func TestUniqueNativeName(t *testing.T) {
	first := uniqueNativeName("format")
	parts := strings.Split(first, "-")
	if len(parts) < 4 {
		t.Fatalf("bad name %q", first)
	}
	if _, err := strconv.Atoi(parts[len(parts)-2]); err != nil {
		t.Fatalf("pid in name %q: %v", first, err)
	}
}
