package volume

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"consonance/internal/agentapi"
)

func TestCatalogRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.json")
	created := time.Date(2026, 10, 7, 3, 15, 0, 0, time.UTC)
	want := catalogEntry{
		RestorePoint: agentapi.RestorePoint{
			ID: "rp-20261007T031500Z-ab12", Branch: "main", CreatedAt: created,
			Reason: agentapi.ReasonAPI, Label: "before schema change", BackupStartLSN: "0/3000028",
		},
		Path: "restore/main/rp-20261007T031500Z-ab12",
	}
	if err := saveCatalog(path, catalog{RestorePoints: []catalogEntry{want}}); err != nil {
		t.Fatal(err)
	}
	got, err := loadCatalog(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 || len(got.RestorePoints) != 1 {
		t.Fatalf("loaded %+v", got)
	}
	entry := got.RestorePoints[0]
	if !entry.CreatedAt.Equal(created) {
		t.Fatalf("createdAt = %v, want %v", entry.CreatedAt, created)
	}
	entry.CreatedAt = created
	if entry != want {
		t.Fatalf("entry = %+v, want %+v", entry, want)
	}

	// The file uses the spec 4.4 field names.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Version       int              `json:"version"`
		RestorePoints []map[string]any `json:"restorePoints"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"id", "branch", "createdAt", "reason", "label", "backupStartLSN", "path"} {
		if _, ok := doc.RestorePoints[0][key]; !ok {
			t.Errorf("catalog entry lacks %q:\n%s", key, raw)
		}
	}
	if doc.RestorePoints[0]["createdAt"] != "2026-10-07T03:15:00Z" {
		t.Errorf("createdAt is written as %v", doc.RestorePoints[0]["createdAt"])
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Error("temporary file left behind")
	}
}

func TestLoadCatalogMissingIsEmpty(t *testing.T) {
	c, err := loadCatalog(filepath.Join(t.TempDir(), "catalog.json"))
	if err != nil || c.Version != 1 || len(c.RestorePoints) != 0 {
		t.Fatalf("loadCatalog = %+v, %v; want an empty version 1 catalog", c, err)
	}
}

func TestLoadCatalogRejectsOtherVersions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(path, []byte(`{"version":2,"restorePoints":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCatalog(path); err == nil {
		t.Fatal("loaded a version 2 catalog")
	}
}

func TestNewRestorePointID(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 15, 0, 0, time.FixedZone("KST", 9*60*60))
	id := newRestorePointID(now)
	if !agentapi.ValidRestorePointID(id) {
		t.Fatalf("id %q has the wrong form", id)
	}
	if !strings.HasPrefix(id, "rp-20261007T031500Z-") {
		t.Fatalf("id %q does not carry the UTC time", id)
	}
	if newRestorePointID(now) == id && newRestorePointID(now) == id {
		t.Fatal("ids do not vary")
	}
}

func TestExpired(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	entry := func(id, branch string, age time.Duration) catalogEntry {
		return catalogEntry{RestorePoint: agentapi.RestorePoint{ID: id, Branch: branch, CreatedAt: now.Add(-age)}}
	}
	entries := []catalogEntry{
		entry("a", "main", time.Hour), entry("b", "main", 2*time.Hour), entry("c", "main", 3*time.Hour),
		entry("d", "main", 30*time.Hour), entry("x", "dev", 40*time.Hour),
	}
	ids := func(es []catalogEntry) []string {
		var out []string
		for _, e := range es {
			out = append(out, e.ID)
		}
		slices.Sort(out)
		return out
	}
	// Two per branch and 24 hours: d is too old, c is the third newest.
	if got := ids(expired(entries, "main", now, 2, 24*time.Hour)); !slices.Equal(got, []string{"c", "d"}) {
		t.Errorf("expired = %v, want [c d]", got)
	}
	if got := expired(entries, "main", now, 0, 0); len(got) != 0 {
		t.Errorf("without limits expired = %v, want none", ids(got))
	}
	if got := ids(expired(entries, "dev", now, 20, 24*time.Hour)); !slices.Equal(got, []string{"x"}) {
		t.Errorf("dev expired = %v, want [x]", got)
	}
}
