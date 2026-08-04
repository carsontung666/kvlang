//go:build !linux || !cgo

package kvshm

import (
	"strings"
	"testing"
)

var _ func(string) error = Destroy

func TestDestroyReportsUnsupportedWithoutCgo(t *testing.T) {
	err := Destroy("test-region")
	if err == nil {
		t.Fatal("Destroy returned nil without Linux+cgo support")
	}
	if !strings.Contains(err.Error(), "requires Linux with cgo enabled") {
		t.Fatalf("Destroy error=%q", err)
	}
}
