// Command consonance-supervisor runs PostgreSQL inside a branch pod and serves
// the internal API used by the volume agent and the operator (spec 7.3).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"consonance/internal/pgctl"
	"consonance/internal/supervisor"
)

type config struct {
	postgresBin    string
	dataDir        string
	socketDir      string
	port           int
	listen         string
	tokenFile      string
	dbPasswordFile string
	maxConnections int
	memoryLimit    int64
	stopGrace      time.Duration
	backupTimeout  time.Duration
	startTimeout   time.Duration
	autostart      bool
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(os.Args[1:]); err != nil {
		slog.Error("supervisor exited", "err", err)
		os.Exit(1)
	}
}

func parseFlags(args []string) (config, error) {
	fs := flag.NewFlagSet("consonance-supervisor", flag.ContinueOnError)
	var c config
	fs.StringVar(&c.postgresBin, "postgres-bin", "/usr/lib/postgresql/17/bin/postgres", "path to the postgres binary")
	fs.StringVar(&c.dataDir, "data-dir", "/pgdata/current", "data directory (the generation symlink)")
	fs.StringVar(&c.socketDir, "socket-dir", "/var/run/postgresql", "unix socket directory")
	fs.IntVar(&c.port, "port", 5432, "postgres port")
	fs.StringVar(&c.listen, "listen", ":8008", "API listen address")
	fs.StringVar(&c.tokenFile, "token-file", "/etc/consonance/token/token", "file holding the internal API token")
	fs.StringVar(&c.dbPasswordFile, "db-password-file", "", "file holding the tenant password; enables /v1/bootstrap (main only)")
	fs.IntVar(&c.maxConnections, "max-connections", 100, "max_connections")
	fs.Int64Var(&c.memoryLimit, "memory-limit-bytes", envInt64("MEMORY_LIMIT_BYTES"), "container memory limit in bytes (Downward API)")
	fs.DurationVar(&c.stopGrace, "stop-grace", 30*time.Second, "fast shutdown grace before immediate shutdown")
	fs.DurationVar(&c.backupTimeout, "backup-timeout", 30*time.Second, "abort a backup session not stopped within this time")
	fs.DurationVar(&c.startTimeout, "start-timeout", 60*time.Second, "how long a start waits for readiness")
	fs.BoolVar(&c.autostart, "autostart", envBool("AUTOSTART", true), "start postgres when the supervisor starts")
	err := fs.Parse(args)
	return c, err
}

func run(args []string) error {
	c, err := parseFlags(args)
	if err != nil {
		return err
	}
	token, err := readSecret(c.tokenFile)
	if err != nil {
		return err
	}

	pg := pgctl.NewManager(c.postgresBin, pgctl.Options{
		DataDir:          c.dataDir,
		SocketDir:        c.socketDir,
		Port:             c.port,
		MemoryLimitBytes: c.memoryLimit,
		MaxConnections:   c.maxConnections,
	})
	srv := &supervisor.Server{
		Token:        token,
		PG:           pg,
		Backups:      pgctl.NewBackups(pg.ConnString(), c.backupTimeout),
		Generation:   func() string { return generationOf(c.dataDir) },
		StopGrace:    c.stopGrace,
		StartTimeout: c.startTimeout,
	}
	if c.dbPasswordFile != "" {
		srv.Bootstrap = func(ctx context.Context) error {
			pw, err := readSecret(c.dbPasswordFile)
			if err != nil {
				return err
			}
			return pgctl.Bootstrap(ctx, pg.ConnString(), pgctl.TenantRole, pgctl.TenantDatabase, pw)
		}
	}

	ln, err := net.Listen("tcp", c.listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", c.listen, err)
	}
	httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = httpSrv.Serve(ln) }()
	slog.Info("supervisor listening", "addr", ln.Addr().String(), "dataDir", c.dataDir)

	// Register the shutdown signals before autostart. A SIGTERM that arrives while postgres is
	// still starting (crash recovery can be slow) is held until autostart returns and is then
	// handled below. Registered later, it would hit Go's default handler, which kills the
	// supervisor and orphans the postmaster.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)

	if c.autostart {
		ctx, cancel := context.WithTimeout(context.Background(), c.startTimeout)
		if err := pg.Start(ctx); err != nil {
			// Keep serving: the operator retries through POST /v1/start.
			slog.Error("autostart failed", "err", err)
		}
		cancel()
	}

	sig := <-sigs
	slog.Info("shutting down", "signal", sig.String())

	ctx, cancel := context.WithTimeout(context.Background(), c.stopGrace+15*time.Second)
	defer cancel()
	stopErr := pg.Stop(ctx, c.stopGrace)
	_ = httpSrv.Shutdown(ctx)
	return stopErr
}

// generationOf returns the generation that the data directory symlink points to.
func generationOf(dataDir string) string {
	target, err := os.Readlink(dataDir)
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}

func readSecret(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return strings.TrimSpace(string(b)), nil
}

func envInt64(name string) int64 {
	v, _ := strconv.ParseInt(os.Getenv(name), 10, 64)
	return v
}

func envBool(name string, def bool) bool {
	v, err := strconv.ParseBool(os.Getenv(name))
	if err != nil {
		return def
	}
	return v
}
