// Command consonance-agent manages one project volume and serves the internal API that the
// operator and the API server use (spec 7.2).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"consonance/internal/agent"
	"consonance/internal/secretfile"
	"consonance/internal/supervisorclient"
	"consonance/internal/volume"
)

var _ agent.Ops = (*volume.Volume)(nil)

type config struct {
	volume          string
	listen          string
	tokenFile       string
	initdb          string
	supervisorURL   string
	projectID       string
	minCopyInterval time.Duration
	lockWait        time.Duration
	maxUsedPercent  float64
	perBranch       int
	maxAge          time.Duration
	retentionEvery  time.Duration
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		slog.Error("agent exited", "err", err)
		os.Exit(1)
	}
}

func parseFlags(args []string) (config, error) {
	fs := flag.NewFlagSet("consonance-agent", flag.ContinueOnError)
	var c config
	fs.StringVar(&c.volume, "volume", "/volume", "project volume root")
	fs.StringVar(&c.listen, "listen", ":8443", "API listen address")
	fs.StringVar(&c.tokenFile, "token-file", "/etc/consonance/token/token", "file holding the project's internal token, for this API and the supervisors'")
	fs.StringVar(&c.initdb, "initdb", "/usr/lib/postgresql/17/bin/initdb", "initdb binary")
	fs.StringVar(&c.supervisorURL, "supervisor-url", "http://br-{branch}:8008", "a branch supervisor's URL; {branch} becomes the branch name")
	fs.StringVar(&c.projectID, "project-id", "", "project ID added to every log line")
	fs.DurationVar(&c.minCopyInterval, "min-copy-interval", 5*time.Second, "minimum time between backup-mode copies of one branch")
	fs.DurationVar(&c.lockWait, "lock-wait", 2*time.Second, "longest wait for another volume operation")
	fs.Float64Var(&c.maxUsedPercent, "max-used-percent", 90, "refuse copies above this volume usage")
	fs.IntVar(&c.perBranch, "restore-points-per-branch", 20, "restore points kept per branch")
	fs.DurationVar(&c.maxAge, "restore-point-max-age", 24*time.Hour, "restore points older than this are removed")
	fs.DurationVar(&c.retentionEvery, "retention-interval", time.Hour, "how often the restore point retention runs")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if fs.NArg() > 0 {
		return c, fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	if !strings.Contains(c.supervisorURL, "{branch}") {
		return c, fmt.Errorf("--supervisor-url %q has no {branch}", c.supervisorURL)
	}
	if c.retentionEvery <= 0 {
		return c, fmt.Errorf("--retention-interval must be positive")
	}
	return c, nil
}

func run(args []string) error {
	// Go exits on SIGHUP by default, and tini would follow. There is nothing to reload.
	signal.Ignore(syscall.SIGHUP)

	c, err := parseFlags(args)
	if err != nil {
		return err
	}
	if c.projectID != "" {
		slog.SetDefault(slog.Default().With("projectId", c.projectID))
	}
	token, err := secretfile.Read(c.tokenFile)
	if err != nil {
		return fmt.Errorf("token file: %w", err)
	}
	hc := &http.Client{Timeout: 60 * time.Second}
	vol, err := volume.Open(volume.Config{
		Root:   c.volume,
		InitDB: c.initdb,
		Supervisor: func(branch string) volume.Supervisor {
			return supervisorclient.New(strings.ReplaceAll(c.supervisorURL, "{branch}", branch), token, hc)
		},
		MinCopyInterval:        c.minCopyInterval,
		LockWait:               c.lockWait,
		MaxUsedPercent:         c.maxUsedPercent,
		RestorePointsPerBranch: c.perBranch,
		RestorePointMaxAge:     c.maxAge,
	})
	if err != nil {
		return err
	}
	removed, err := vol.Recover()
	if err != nil {
		return fmt.Errorf("recover: %w", err)
	}
	if len(removed) > 0 {
		slog.Warn("removed what interrupted operations left", "paths", removed)
	}

	ln, err := net.Listen("tcp", c.listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", c.listen, err)
	}
	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	janitorCtx, stopJanitor := context.WithCancel(context.Background())
	janitorDone := make(chan struct{})
	go func() {
		defer close(janitorDone)
		vol.RunJanitor(janitorCtx, c.retentionEvery)
	}()
	defer func() {
		stopJanitor()
		<-janitorDone
	}()

	httpSrv := &http.Server{Handler: (&agent.Server{Token: token, Ops: vol}).Handler(), ReadHeaderTimeout: 5 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(ln) }()
	slog.Info("agent listening", "addr", ln.Addr().String(), "volume", c.volume)

	select {
	case <-sigCtx.Done():
		slog.Info("shutting down")
	case err := <-serveErr:
		return fmt.Errorf("serve: %w", err)
	}
	// Let running operations finish: a copy takes about a second, and Kubernetes waits 30s
	// before it kills the pod.
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	return httpSrv.Shutdown(ctx)
}
