package secretfile_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"consonance/internal/secretfile"
)

func TestRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("  s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := secretfile.Read(path); err != nil || got != "s3cret" {
		t.Fatalf("Read = %q, %v; want s3cret", got, err)
	}
	if err := os.WriteFile(path, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := secretfile.Read(path); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("empty secret: %v, want an 'is empty' error", err)
	}
	if _, err := secretfile.Read(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing file was read")
	}
}
