// Command gitea-k8s-runner-plugin is a plugin.v1alpha backend that runs each
// workflow job of a Gitea (or Forgejo) runner as a Kubernetes Job.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	pluginv1 "github.com/perfectra1n/gitea-k8s-runner-plugin/gen/plugin/v1alpha"
	"github.com/perfectra1n/gitea-k8s-runner-plugin/plugin/internal/k8s"
	"github.com/perfectra1n/gitea-k8s-runner-plugin/plugin/internal/options"
	"github.com/perfectra1n/gitea-k8s-runner-plugin/plugin/internal/server"
)

const saNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// shutdownTimeout bounds removing live environments on SIGTERM.
const shutdownTimeout = 60 * time.Second

type config struct {
	listen     string
	kubeconfig string
	socketMode os.FileMode
	defaults   options.Defaults
}

func main() { os.Exit(realMain()) }

func realMain() int {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg, err := parseFlags(os.Args[1:], envWithFallbacks)
	if err != nil {
		log.Error("invalid flags", "err", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := run(ctx, cfg, log); err != nil {
		log.Error("plugin failed", "err", err)
		return 1
	}
	return 0
}

// envWithFallbacks resolves POD_NAMESPACE from the service account and
// POD_NAME from the hostname when the downward API does not set them.
func envWithFallbacks(k string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	switch k {
	case "POD_NAMESPACE":
		b, _ := os.ReadFile(saNamespaceFile)
		return strings.TrimSpace(string(b))
	case "POD_NAME":
		h, _ := os.Hostname()
		return h
	}
	return ""
}

func parseFlags(args []string, getenv func(string) string) (config, error) {
	fs := flag.NewFlagSet("gitea-k8s-runner-plugin", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var cfg config
	var mode string
	fs.StringVar(&cfg.listen, "listen", "unix:///run/plugin/plugin.sock", "listen address: unix:///path or tcp://host:port")
	fs.StringVar(&cfg.kubeconfig, "kubeconfig", "", "kubeconfig path (default: in-cluster config)")
	fs.StringVar(&cfg.defaults.Namespace, "namespace", getenv("POD_NAMESPACE"), "namespace for job pods (default: the plugin's own)")
	fs.StringVar(&cfg.defaults.Instance, "instance", getenv("POD_NAME"), "owner label value for orphan sweeping (default: the pod name)")
	fs.StringVar(&cfg.defaults.Podspec, "podspec", "", "default podspec path when a label has no argument")
	fs.DurationVar(&cfg.defaults.ReadyTimeout, "ready-timeout", options.DefaultReadyTimeout, "default create-to-ready bound")
	fs.StringVar(&mode, "socket-mode", "0660", "permissions of the unix socket")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if fs.NArg() > 0 {
		return config{}, fmt.Errorf("unexpected arguments: %q", fs.Args())
	}
	if cfg.defaults.Namespace == "" {
		return config{}, errors.New("no namespace: pass --namespace or set POD_NAMESPACE")
	}
	if cfg.defaults.Instance == "" {
		return config{}, errors.New("no instance: pass --instance or set POD_NAME")
	}
	m, err := strconv.ParseUint(mode, 8, 32)
	if err != nil {
		return config{}, fmt.Errorf("--socket-mode: %w", err)
	}
	cfg.socketMode = os.FileMode(m)
	if _, _, err := parseListen(cfg.listen); err != nil {
		return config{}, err
	}
	return cfg, nil
}

func parseListen(s string) (network, addr string, err error) {
	switch {
	case strings.HasPrefix(s, "unix://"):
		addr = strings.TrimPrefix(s, "unix://")
		if !filepath.IsAbs(addr) {
			return "", "", fmt.Errorf("--listen %q: unix socket path must be absolute", s)
		}
		return "unix", addr, nil
	case strings.HasPrefix(s, "tcp://"):
		addr = strings.TrimPrefix(s, "tcp://")
		if addr == "" {
			return "", "", fmt.Errorf("--listen %q: missing host:port", s)
		}
		return "tcp", addr, nil
	}
	return "", "", fmt.Errorf("--listen %q: want unix:///path or tcp://host:port", s)
}

func run(ctx context.Context, cfg config, log *slog.Logger) error {
	restCfg, err := restConfig(cfg.kubeconfig)
	if err != nil {
		return err
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return err
	}
	backend, err := k8s.NewBackend(cs, restCfg, cfg.defaults.Namespace)
	if err != nil {
		return err
	}

	// A previous plugin process in this pod may have died with jobs running;
	// the runner restarted too, so nothing will ever Remove them.
	if n, err := backend.Sweep(ctx, cfg.defaults.Instance); err != nil {
		log.Warn("orphan sweep incomplete", "err", err, "deleted", n)
	} else if n > 0 {
		log.Info("deleted orphan jobs", "count", n, "instance", cfg.defaults.Instance)
	}

	lis, err := listen(cfg)
	if err != nil {
		return err
	}
	srv := server.New(backend, cfg.defaults)
	gs := grpc.NewServer()
	pluginv1.RegisterBackendPluginServer(gs, srv)
	hs := health.NewServer()
	healthpb.RegisterHealthServer(gs, hs)

	serveErr := make(chan error, 1)
	go func() { serveErr <- gs.Serve(lis) }()
	log.Info("serving", "listen", cfg.listen, "namespace", cfg.defaults.Namespace, "instance", cfg.defaults.Instance)

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down: letting in-flight calls finish, then removing live environments")
	hs.Shutdown()
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	// Stop taking calls and let running ones (a step mid-exec) finish, for
	// most of the budget; then remove whatever environments are left.
	stopped := make(chan struct{})
	go func() { gs.GracefulStop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(shutdownTimeout * 3 / 4):
	}
	srv.Shutdown(sctx)
	gs.Stop()
	return nil
}

func restConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}

func listen(cfg config) (net.Listener, error) {
	network, addr, err := parseListen(cfg.listen)
	if err != nil {
		return nil, err
	}
	if network == "unix" {
		if err := os.MkdirAll(filepath.Dir(addr), 0o750); err != nil {
			return nil, err
		}
		// A socket left by a previous process blocks bind.
		if err := os.Remove(addr); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	lis, err := net.Listen(network, addr)
	if err != nil {
		return nil, err
	}
	if network == "unix" {
		if err := os.Chmod(addr, cfg.socketMode); err != nil {
			_ = lis.Close()
			return nil, err
		}
	}
	return lis, nil
}
