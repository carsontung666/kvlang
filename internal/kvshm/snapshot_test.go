package kvshm

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

func TestNativeSnapshotManifest(t *testing.T) {
	manifest, err := os.Open("SHA256SUMS")
	if err != nil {
		t.Fatal(err)
	}
	defer manifest.Close()

	seen := 0
	scanner := bufio.NewScanner(manifest)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			t.Fatalf("invalid SHA256SUMS line %q", scanner.Text())
		}
		want, err := hex.DecodeString(fields[0])
		if err != nil || len(want) != sha256.Size {
			t.Fatalf("invalid digest %q", fields[0])
		}
		contents, err := os.ReadFile(fields[1])
		if err != nil {
			t.Fatal(err)
		}
		got := sha256.Sum256(contents)
		if !strings.EqualFold(hex.EncodeToString(got[:]), fields[0]) {
			t.Fatalf("snapshot digest mismatch for %s", fields[1])
		}
		seen++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if seen != 27 {
		t.Fatalf("manifest contains %d files, want 27", seen)
	}
}
