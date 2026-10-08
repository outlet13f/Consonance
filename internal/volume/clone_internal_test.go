package volume

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCloneFileSkipsAMissingSource(t *testing.T) {
	src := filepath.Join(t.TempDir(), "missing")
	dst := filepath.Join(t.TempDir(), "target")

	// cloneFile should return nil when the source doesn't exist
	err := cloneFile(src, dst, 0o600)
	if err != nil {
		t.Errorf("cloneFile(missing source) = %v, want nil", err)
	}

	// dst should not exist after cloneFile returns nil
	if _, err := os.Lstat(dst); err == nil {
		t.Error("dst exists, but should not")
	} else if !os.IsNotExist(err) {
		t.Errorf("Lstat(dst): %v", err)
	}
}
