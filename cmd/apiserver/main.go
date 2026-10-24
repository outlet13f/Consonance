// Command consonance-apiserver is the REST API server (spec chapter 5). It runs in
// consonance-system, in front of the operator: it reads and writes Projects and Branches, and
// reaches a project's volume agent through its service.
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
	"syscall"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"consonance/api/v1alpha1"
	"consonance/internal/httpapi"
	"consonance/internal/secretfile"
)

const (
	// readHeaderTimeout bounds a client that opens a connection and sends nothing.
	readHeaderTimeout = 10 * time.Second
	// shutdownTimeout is how long a stop waits for requests under way: longer than the 30s a
	// rollback or reset holds its response (spec 14), so none of them is cut off.
	shutdownTimeout = 35 * time.Second
	// cacheSyncTimeout is how long the start waits for the first list of Projects and Branches.
	cacheSyncTimeout = 30 * time.Second
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		slog.Error("API server exited", "err", err)
		os.Exit(1)
	}
}

type config struct {
	listenAddress string
	adminKeyFile  string
	storageClass  string
	kubeconfig    string
}

func parseFlags(args []string) (config, error) {
	fs := flag.NewFlagSet("consonance-apiserver", flag.ContinueOnError)
	var c config
	fs.StringVar(&c.listenAddress, "listen-address", ":8080", "address the API listens on")
	fs.StringVar(&c.adminKeyFile, "admin-key-file", "", "file with the admin key (the Secret consonance-admin-key, mounted); required")
	fs.StringVar(&c.storageClass, "storage-class", "consonance-xfs", "storageClassName of new projects")
	fs.StringVar(&c.kubeconfig, "kubeconfig", "", "kubeconfig file; empty means $KUBECONFIG, then in-cluster, then ~/.kube/config")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if fs.NArg() > 0 {
		return c, fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	if c.adminKeyFile == "" {
		return c, errors.New("--admin-key-file is required")
	}
	return c, nil
}

func run(args []string) error {
	c, err := parseFlags(args)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)
	ctrl.SetLogger(logr.FromSlogHandler(logger.Handler()))

	// An empty key would make every request a 401: better not to start.
	key, err := secretfile.Read(c.adminKeyFile)
	if err != nil {
		return err
	}
	cfg, err := restConfig(c.kubeconfig)
	if err != nil {
		return err
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return err
	}

	// SIGTERM or SIGINT stops the server. The cache has a context of its own, which ends after
	// the server has: requests under way during the shutdown (a rollback waits on it) go on
	// reading a live cache.
	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	cacheCtx, stopCache := context.WithCancel(context.Background())
	defer stopCache()

	cached, reader, err := newClients(cacheCtx, cfg, scheme)
	if err != nil {
		return err
	}
	handler := httpapi.New(httpapi.Config{
		Client:       cached,
		Reader:       reader,
		Agents:       httpapi.HTTPAgents{HTTP: &http.Client{}},
		AdminKey:     key,
		StorageClass: c.storageClass,
		Log:          logger,
	})
	// There is no WriteTimeout: a rollback may hold its response for 30s, and a limit that cut
	// it off would answer nothing for work that went on.
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: readHeaderTimeout}
	ln, err := net.Listen("tcp", c.listenAddress)
	if err != nil {
		return err
	}
	logger.Info("API server starting", "address", ln.Addr().String(), "storageClass", c.storageClass)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	select {
	case err := <-serveErr:
		return err
	case <-sigCtx.Done():
	}
	// Restore the default handling, so that a second signal ends the process at once.
	stop()
	logger.Info("API server stopping")
	shutdown, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		_ = srv.Close()
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// newClients makes the cached client and the uncached reader. Only Projects and Branches are
// cached (the informers are started and synced here, so a missing permission stops the start and
// the first request does not wait); Secrets and Namespaces go to the cluster each time: the
// cache would need list and watch on every Secret in the cluster, and a namespace check must see
// what the cluster has now.
func newClients(ctx context.Context, cfg *rest.Config, scheme *runtime.Scheme) (client.Client, client.Reader, error) {
	c, err := cache.New(cfg, cache.Options{Scheme: scheme})
	if err != nil {
		return nil, nil, fmt.Errorf("cache: %w", err)
	}
	go func() {
		if err := c.Start(ctx); err != nil {
			slog.Error("cache stopped", "err", err)
		}
	}()
	syncCtx, cancel := context.WithTimeout(ctx, cacheSyncTimeout)
	defer cancel()
	for _, obj := range []client.Object{&v1alpha1.Project{}, &v1alpha1.Branch{}} {
		if _, err := c.GetInformer(syncCtx, obj); err != nil {
			return nil, nil, fmt.Errorf("cache %T: %w", obj, err)
		}
	}
	// GetInformer can return before Start has run, and then it does not wait for the sync.
	if !c.WaitForCacheSync(syncCtx) {
		return nil, nil, errors.New("cache did not sync")
	}
	cached, err := client.New(cfg, client.Options{
		Scheme: scheme,
		Cache: &client.CacheOptions{
			Reader:     c,
			DisableFor: []client.Object{&corev1.Secret{}, &corev1.Namespace{}},
		},
	})
	if err != nil {
		return nil, nil, err
	}
	reader, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, nil, err
	}
	return cached, reader, nil
}

func restConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return ctrl.GetConfig()
}
