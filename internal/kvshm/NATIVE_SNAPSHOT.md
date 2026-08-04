# Native snapshot

`native/include` and `native/src` are an exact source snapshot of the sibling
`kvspace-c` implementation. The Go package compiles that snapshot directly;
it does not link against a developer machine path or a prebuilt architecture-
specific archive.

Keep the six public headers, twelve C++ translation units, and nine private
headers synchronized when the native ABI or storage layout changes. The
`SHA256SUMS` manifest is checked by the Go test suite so an incomplete or
accidental vendor edit cannot pass silently.

On x86_64 Linux glibc with cgo, `client_cgo.go` wraps the C ABI and registers
these DSNs:

- `shm-art-bump://NAME`
- `shm-art-box://NAME`
- `shm-hash-box://NAME`
- `shm-trie-box://NAME`

The wrapper compiles the vendored sources using C++17. The mapped-object
lifetime helper remains a separate, optimized, non-LTO translation unit, and
the native consumers compile without strict-aliasing assumptions. The package
links only the C++ runtime, pthreads, and POSIX realtime support. Non-Linux and
`CGO_ENABLED=0` builds retain registration but fail clearly only if a
shared-memory DSN is selected.
