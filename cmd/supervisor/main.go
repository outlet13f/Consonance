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
	"strconv"
	"syscall"
	"time"

	"consonance/internal/pgctl"
	"consonance/internal/secretfile"
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
	// Go exits on SIGHUP by default, and in the image tini would follow and postgres would be
	// SIGKILLed. A SIGHUP ("reload", a habit with postgres containers) has nothing to reload here:
	// every setting is passed on the command line.
	signal.Ignore(syscall.SIGHUP)

	c, err := parseFlags(args)
	if err != nil {
		return err
	}
	token, err := secretfile.Read(c.tokenFile)
	if err != nil {
		return fmt.Errorf("token file: %w", err)
	}
	if c.dbPasswordFile != "" {
		// Bootstrap reads the file again on every call; this only makes a bad one fail at startup.
		if _, err := secretfile.Read(c.dbPasswordFile); err != nil {
			return fmt.Errorf("tenant password file: %w", err)
		}
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
		Generation:   pg.Generation,
		StopGrace:    c.stopGrace,
		StartTimeout: c.startTimeout,
	}
	if c.dbPasswordFile != "" {
		srv.Bootstrap = func(ctx context.Context) error {
			pw, err := secretfile.Read(c.dbPasswordFile)
			if err != nil {
				return fmt.Errorf("tenant password file: %w", err)
			}
			return pgctl.Bootstrap(ctx, pg.ConnString(), pgctl.TenantRole, pgctl.TenantDatabase, pw)
		}
	}

	ln, err := net.Listen("tcp", c.listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", c.listen, err)
	}

	// Register the shutdown signals before the API starts serving: from then on POST /v1/start
	// can launch a postmaster, and a SIGTERM that reached Go's default handler would kill the
	// supervisor and orphan it. The signal also cancels sigCtx, which cuts the autostart wait
	// short. A pod deleted while postgres is still starting (slow crash recovery) then gets its
	// fast shutdown at once, not after --start-timeout, which can outlast the pod's grace period.
	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = httpSrv.Serve(ln) }()
	slog.Info("supervisor listening", "addr", ln.Addr().String(), "dataDir", c.dataDir)

	if c.autostart {
		// The context only bounds the wait. When the signal cancels it, Start returns but the
		// postmaster keeps starting, and the Stop below sends it the fast shutdown.
		ctx, cancel := context.WithTimeout(sigCtx, c.startTimeout)
		if err := pg.Start(ctx); err != nil && sigCtx.Err() == nil {
			// Keep serving: the operator retries through POST /v1/start.
			slog.Error("autostart failed", "err", err)
		}
		cancel()
	}

	<-sigCtx.Done()
	slog.Info("shutting down")

	// Not derived from sigCtx, which is already cancelled: Stop escalates to an immediate
	// shutdown as soon as its context ends.
	ctx, cancel := context.WithTimeout(context.Background(), c.stopGrace+15*time.Second)
	defer cancel()
	stopErr := pg.Stop(ctx, c.stopGrace)
	_ = httpSrv.Shutdown(ctx)
	return stopErr
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
