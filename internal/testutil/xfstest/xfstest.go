// Package xfstest provides scratch directories on an XFS filesystem with reflink for tests.
package xfstest

import (
	"os"
	"testing"
)

// EnvDir names the environment variable pointing at a writable directory on XFS with reflink
// (spec 12). hack/pg-test.sh sets it in the test container on the Podman machine.
const EnvDir = "CONSONANCE_TEST_XFS_DIR"

// Dir returns a fresh directory under $CONSONANCE_TEST_XFS_DIR, removed when the test ends,
// or skips the test when the variable is not set.
func Dir(t testing.TB) string {
	t.Helper()
	base := os.Getenv(EnvDir)
	if base == "" {
		t.Skipf("%s not set; skipping test that needs XFS with reflink", EnvDir)
	}
	dir, err := os.MkdirTemp(base, "t-")
	if err != nil {
		t.Fatalf("mkdir under %s: %v", base, err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}
