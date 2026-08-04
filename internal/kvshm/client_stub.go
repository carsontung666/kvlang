//go:build !linux || !cgo

package kvshm

import (
	"fmt"

	"github.com/array2d/kvspace-go"
)

func openNative(_ string, selected engine) kvspace.KVSpace {
	panic(fmt.Sprintf(
		"kvlang: shm engine %d requires Linux with cgo enabled", selected))
}

// Destroy is unavailable with the native shared-memory backend disabled.
func Destroy(name string) error {
	return fmt.Errorf(
		"kvlang: cannot destroy shared-memory region %q: requires Linux with cgo enabled",
		name,
	)
}
