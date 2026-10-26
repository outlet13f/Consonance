// Command consonance-operator reconciles Projects and Branches (spec 7.1). It runs in
// consonance-system and reaches the agents and supervisors through their services.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"consonance/api/v1alpha1"
	"consonance/internal/controller"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		slog.Error("operator exited", "err", err)
		os.Exit(1)
	}
}

type config struct {
	agentImage    string
	postgresImage string
	metricsAddr   string
	probeAddr     string
	kubeconfig    string
	leaderElect   bool
	leaderNS      string
}

func parseFlags(args []string) (config, error) {
	fs := flag.NewFlagSet("consonance-operator", flag.ContinueOnError)
	var c config
	fs.StringVar(&c.agentImage, "agent-image", "consonance/agent:dev", "volume agent image")
	fs.StringVar(&c.postgresImage, "postgres-image", "consonance/postgres:17-dev", "branch image (PostgreSQL and the supervisor)")
	fs.StringVar(&c.metricsAddr, "metrics-bind-address", ":8080", "metrics listen address; 0 turns metrics off")
	fs.StringVar(&c.probeAddr, "health-probe-bind-address", ":8081", "liveness and readiness listen address")
	fs.StringVar(&c.kubeconfig, "kubeconfig", "", "kubeconfig file; empty means $KUBECONFIG, then in-cluster, then ~/.kube/config")
	fs.BoolVar(&c.leaderElect, "leader-elect", false, "elect one leader among replicas")
	fs.StringVar(&c.leaderNS, "leader-election-namespace", "consonance-system", "namespace of the leader election lease (used only with --leader-elect)")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if fs.NArg() > 0 {
		return c, fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	return c, nil
}

// leaderNamespace is the namespace of the lease, set only when leader election is on: the lease
// and its Role are in consonance-system, not wherever the pod happens to run (out of cluster,
// controller-runtime would otherwise have no namespace to use).
func leaderNamespace(c config) string {
	if !c.leaderElect {
		return ""
	}
	return c.leaderNS
}

func run(args []string) error {
	c, err := parseFlags(args)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)
	ctrl.SetLogger(logr.FromSlogHandler(logger.Handler()))

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
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: c.metricsAddr},
		HealthProbeBindAddress: c.probeAddr,
		LeaderElection:         c.leaderElect,
		LeaderElectionID:       "consonance-operator",
		// Empty outside --leader-elect: controller-runtime then reads no namespace at all.
		LeaderElectionNamespace: leaderNamespace(c),
		Cache:                   controller.CacheOptions(),
	})
	if err != nil {
		return fmt.Errorf("manager: %w", err)
	}
	conn := controller.NewHTTPConnector()
	if err := (&controller.ProjectReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Connector: conn, AgentImage: c.agentImage,
	}).SetupWithManager(mgr); err != nil {
		return err
	}
	if err := (&controller.BranchReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Connector: conn, PostgresImage: c.postgresImage,
	}).SetupWithManager(mgr); err != nil {
		return err
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}
	slog.Info("operator starting", "agentImage", c.agentImage, "postgresImage", c.postgresImage)
	// SIGTERM or SIGINT stops the manager: it cancels the running reconciles and waits for them
	// to return. A step cut short goes on from the status after the restart (spec 10).
	return mgr.Start(ctrl.SetupSignalHandler())
}

func restConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return ctrl.GetConfig()
}
