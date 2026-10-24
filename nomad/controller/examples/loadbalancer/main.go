// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Command loadbalancer is the sample operator of the Norca controller SDK:
// the equivalent of the Kubernetes LoadBalancer service type.
//
// A LoadBalancer object names a Nomad service and a frontend port; the
// controller keeps a load balancer forwarding to the healthy instances of
// the service and reports its addresses and conditions in the status of the
// object.
//
// With -objects=variables (the default) objects are stored as Variables at
// crd/norca-io/LoadBalancer/<name>:
//
//	norca var put crd/norca-io/LoadBalancer/web \
//	  spec='{"service":"web","port":8080}'
//	norca var get -item=status crd/norca-io/LoadBalancer/web
//
// With -objects=native they are native objects of the kind
// norca.io/LoadBalancer, registered from loadbalancer.kind.hcl: the server
// validates the spec against loadbalancer.schema.json, fills in its default
// values and maintains the generation, and the controller writes the status
// on the status endpoint (the "write-status" capability):
//
//	norca object-kind apply loadbalancer.kind.hcl
//	norca object apply web.hcl
//	norca object list norca.io/LoadBalancer
//
// Objects are also created from services marked with the norca_lb_* service
// meta keys (see discovery.go), and deleted when the mark is removed.
//
// The "nomad" class, the only provider of the prototype, runs the load
// balancer as a Nomad job (lb-<name>) owned by the object: a TCP proxy (the
// "proxy" subcommand of this binary) whose targets come from the job's
// Variable (nomad/jobs/lb-<name>), which the controller updates when the
// service instances change. Deleting the object deletes the job.
//
// Subcommands:
//
//	loadbalancer [controller] [flags]   run the controller (default)
//	loadbalancer proxy -listen :8080 -targets local/targets
//
// See loadbalancer.nomad.hcl and loadbalancer-policy.hcl for running the
// controller as a job.
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
	"sync"
	"syscall"
	"time"

	"github.com/hashicorp/nomad/controller"
)

func main() {
	args := os.Args[1:]
	cmd := "controller"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd {
	case "controller":
		err = runController(ctx, args)
	case "proxy":
		err = runProxy(ctx, args)
	default:
		err = fmt.Errorf("unknown subcommand %q (use controller or proxy)", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newLogger(level string) (*slog.Logger, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		return nil, err
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l})), nil
}

func runProxy(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
	listen := fs.String("listen", ":8080", "address to listen on")
	targets := fs.String("targets", "local/targets", "file with one target host:port per line")
	logLevel := fs.String("log-level", "info", "log level")
	if err := fs.Parse(args); err != nil {
		return err
	}
	logger, err := newLogger(*logLevel)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	logger.Info("proxy listening", "address", ln.Addr().String(), "targets", *targets)
	p := &proxy{targetsFile: *targets, logger: logger, dialTimeout: 3 * time.Second}
	return p.serve(ctx, ln)
}

type controllerFlags struct {
	objects        string
	namespace      string
	watchMode      string
	workers        int
	leaderElection bool
	leaseNamespace string
	discover       bool
	metricsAddr    string
	logLevel       string
	resync         time.Duration
	provider       NomadProviderConfig
}

func runController(ctx context.Context, args []string) error {
	self, _ := os.Executable()
	var f controllerFlags
	fs := flag.NewFlagSet("controller", flag.ContinueOnError)
	fs.StringVar(&f.objects, "objects", "variables", `storage of the LoadBalancer objects: "variables" or "native" (kind norca.io/LoadBalancer)`)
	fs.StringVar(&f.namespace, "namespace", "default", `namespace of the LoadBalancer objects ("*" for all)`)
	fs.StringVar(&f.watchMode, "watch-mode", "", `"stream", "poll", or empty for the event stream with fallback to polling`)
	fs.IntVar(&f.workers, "workers", 2, "concurrent reconciles")
	fs.BoolVar(&f.leaderElection, "leader-election", true, "run reconciles only on the elected leader")
	fs.StringVar(&f.leaseNamespace, "lease-namespace", "", "namespace of the lease variables (default: -namespace, or default)")
	fs.BoolVar(&f.discover, "discover-services", true, "create LoadBalancer objects from marked services")
	fs.StringVar(&f.metricsAddr, "metrics-addr", "", "address of the /metrics endpoint (disabled if empty)")
	fs.StringVar(&f.logLevel, "log-level", "info", "log level")
	fs.DurationVar(&f.resync, "resync", 10*time.Minute, "period of full resynchronization")
	fs.StringVar(&f.provider.JobPrefix, "job-prefix", "lb-", "prefix of the proxy job IDs")
	fs.StringVar(&f.provider.Driver, "proxy-driver", "raw_exec", "task driver of the proxy jobs: raw_exec, exec or docker")
	fs.StringVar(&f.provider.Command, "proxy-command", self, "path of this binary on the nodes (raw_exec, exec)")
	fs.StringVar(&f.provider.Image, "proxy-image", "", "container image with this binary (docker)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	logger, err := newLogger(f.logLevel)
	if err != nil {
		return err
	}
	slog.SetDefault(logger)
	storage, err := controller.ParseStorage(f.objects)
	if err != nil {
		return err
	}

	client, err := controller.NewAPIClient()
	if err != nil {
		return err
	}
	backend := controller.NewBackend(client)
	cluster := &nomadCluster{client: client}
	nomadProv, err := newNomadProvider(f.provider, backend, cluster)
	if err != nil {
		return err
	}
	metrics := controller.NewTextMetrics()
	if f.metricsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", metrics)
		srv := &http.Server{Addr: f.metricsAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("metrics endpoint failed", "error", err)
			}
		}()
		go func() {
			<-ctx.Done()
			_ = srv.Close()
		}()
	}

	leaseNS := f.leaseNamespace
	if leaseNS == "" && f.namespace != "*" {
		leaseNS = f.namespace
	}
	election := func() *controller.LeaderElectionConfig {
		if !f.leaderElection {
			return nil
		}
		return &controller.LeaderElectionConfig{Namespace: leaseNS, ReleaseOnCancel: true}
	}

	lb := &reconciler{
		cluster:   cluster,
		providers: map[string]Provider{nomadProv.Class(): nomadProv},
		logger:    logger,
		now:       time.Now,

		requeueNotReady: 15 * time.Second,
		requeueReady:    time.Minute,
	}
	lbCtrl, err := controller.New(backend, lb, controller.Options{
		Name:           "loadbalancer",
		Kind:           Kind,
		Storage:        storage,
		Namespace:      f.namespace,
		Workers:        f.workers,
		ResyncPeriod:   f.resync,
		WatchMode:      controller.WatchMode(f.watchMode),
		OwnsJobs:       true,
		Sources:        []controller.Source{serviceSource()},
		LeaderElection: election(),
		Metrics:        metrics,
		Logger:         logger,
	})
	if err != nil {
		return err
	}
	lb.ctrl = lbCtrl
	controllers := []*controller.Controller{lbCtrl}

	if f.discover {
		index := &markIndex{byJob: map[string]map[string]mark{}}
		disc := &discoveryReconciler{index: index, logger: logger.With("controller", "loadbalancer-discovery")}
		discCtrl, err := controller.New(backend, disc, controller.Options{
			Name:           "loadbalancer-discovery",
			Kind:           Kind,
			Storage:        storage,
			Namespace:      f.namespace,
			ResyncPeriod:   f.resync,
			WatchMode:      controller.WatchMode(f.watchMode),
			Sources:        []controller.Source{&markSource{index: index, backend: backend, ns: f.namespace}},
			Predicate:      controller.AnyChange,
			LeaderElection: election(),
			Metrics:        metrics,
			Logger:         logger,
		})
		if err != nil {
			return err
		}
		disc.ctrl = discCtrl
		controllers = append(controllers, discCtrl)
	}

	var wg sync.WaitGroup
	errs := make([]error, len(controllers))
	for i, c := range controllers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = c.Run(ctx)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
