package pgctl_test

import (
	"slices"
	"testing"

	"consonance/internal/pgctl"
)

func TestArgsIncludeRuntimeSettings(t *testing.T) {
	got := pgctl.Args(pgctl.Options{
		DataDir:          "/pgdata/current",
		SocketDir:        "/var/run/postgresql",
		Port:             5432,
		MemoryLimitBytes: 1 << 30,
		MaxConnections:   100,
	})
	want := []string{
		"-D", "/pgdata/current",
		"-c", "port=5432",
		"-c", "listen_addresses=*",
		"-c", "unix_socket_directories=/var/run/postgresql",
		"-c", "shared_buffers=256MB",
		"-c", "max_connections=100",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Args() =\n%q\nwant\n%q", got, want)
	}
}

func TestSharedBuffersIsQuarterOfMemoryLimit(t *testing.T) {
	cases := []struct {
		limit int64
		want  string
	}{
		{0, "128MB"},         // unknown limit: PostgreSQL's default
		{1 << 30, "256MB"},   // 1GiB
		{2 << 30, "512MB"},   // 2GiB
		{32 << 20, "16MB"},   // 32MiB -> 8MB, clamped up to 16MB
		{64 << 30, "8192MB"}, // 64GiB -> 16GB, clamped down to 8GB
	}
	for _, c := range cases {
		if got := pgctl.SharedBuffers(c.limit); got != c.want {
			t.Errorf("SharedBuffers(%d) = %s, want %s", c.limit, got, c.want)
		}
	}
}
