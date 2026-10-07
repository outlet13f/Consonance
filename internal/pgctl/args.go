// Package pgctl runs and controls one PostgreSQL server process.
package pgctl

import "fmt"

// Options configure how the postmaster is launched.
type Options struct {
	DataDir          string // data directory, usually the generation symlink /pgdata/current
	SocketDir        string // unix_socket_directories
	Port             int
	MemoryLimitBytes int64 // container memory limit; 0 when unknown
	MaxConnections   int
}

// Args returns the postgres command-line arguments. All runtime settings are passed
// with -c so branches copied from one another never inherit each other's settings.
func Args(o Options) []string {
	return []string{
		"-D", o.DataDir,
		"-c", fmt.Sprintf("port=%d", o.Port),
		"-c", "listen_addresses=*",
		"-c", "unix_socket_directories=" + o.SocketDir,
		"-c", "shared_buffers=" + SharedBuffers(o.MemoryLimitBytes),
		"-c", fmt.Sprintf("max_connections=%d", o.MaxConnections),
	}
}

// SharedBuffers is 25% of the memory limit in MB, clamped to [16MB, 8192MB].
// It returns PostgreSQL's default of 128MB when the limit is unknown.
func SharedBuffers(limitBytes int64) string {
	if limitBytes <= 0 {
		return "128MB"
	}
	mb := limitBytes / 4 / (1 << 20)
	mb = max(mb, 16)
	mb = min(mb, 8192)
	return fmt.Sprintf("%dMB", mb)
}
