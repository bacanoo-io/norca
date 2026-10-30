// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package natsbridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/controller"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Run runs the bridge command with cfg until ctx is canceled, then shuts
// down gracefully: the batch being published is finished or abandoned (it
// is published again by the next leader), the checkpoint is stored, the
// lease is released and the NATS connection is closed.
func Run(ctx context.Context, cfg *Config, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	client, apiRegion, err := newAPIClient(cfg.Addr, cfg.Token)
	if err != nil {
		return fmt.Errorf("creating the API client: %w", err)
	}
	region := cfg.Region
	if region == "" {
		region = apiRegion
	}
	if region == "" {
		if region, err = client.Agent().Region(); err != nil {
			return fmt.Errorf("the region is not set (-region) and cannot be read from the agent: %w", err)
		}
	}

	statePrefix := cfg.StatePrefix
	if statePrefix == "" {
		statePrefix = "natsbridge/" + region
	}
	vars := controller.NewBackend(client)

	identity := cfg.Identity
	var le *controller.LeaderElector
	leasePath := ""
	if cfg.LeaderElect {
		leasePath = cfg.LeasePath
		if leasePath == "" {
			leasePath = statePrefix + "/lease"
		}
		le, err = controller.NewLeaderElector(vars, controller.LeaderElectionConfig{
			Namespace:       cfg.StateNamespace,
			Path:            leasePath,
			Identity:        identity,
			LeaseDuration:   cfg.LeaseDuration,
			RenewDeadline:   cfg.LeaseDuration * 3 / 4,
			RetryPeriod:     cfg.LeaseDuration / 5,
			ReleaseOnCancel: true,
		}, logger)
		if err != nil {
			return err
		}
		identity = le.Identity()
	}
	if identity == "" {
		host, _ := os.Hostname()
		identity = fmt.Sprintf("%s-%d", host, os.Getpid())
	}

	var cp Checkpointer
	checkpointPath := ""
	switch cfg.Checkpoint {
	case CheckpointVariable:
		checkpointPath = cfg.CheckpointPath
		if checkpointPath == "" {
			checkpointPath = statePrefix + "/checkpoint"
		}
		cp = &VariableCheckpoint{Vars: vars, Namespace: cfg.StateNamespace, Path: checkpointPath, Region: region, Writer: identity}
	case CheckpointFile:
		cp = &FileCheckpoint{Path: cfg.CheckpointPath, Region: region}
	default:
		cp = &MemoryCheckpoint{}
	}

	nc, err := connectNATS(cfg, region, logger)
	if err != nil {
		return err
	}
	defer nc.Close()

	if cfg.SyncPushes || cfg.SyncRegions {
		syncer := &Syncer{API: &apiRegistries{client: client, region: region}, Region: region, Cooldown: cfg.SyncCooldown, Logger: logger}
		subs, err := syncer.Subscribe(ctx, nc, cfg.SubjectPrefix, cfg.SyncPushes, cfg.SyncRegions)
		for _, sub := range subs {
			defer sub.Unsubscribe() //nolint:errcheck
		}
		if err != nil {
			return err
		}
		logger.Info("syncing pack registries from NATS", "pushes", cfg.SyncPushes, "regions", cfg.SyncRegions)
	}

	var pub Publisher = &CorePublisher{Conn: nc, Timeout: cfg.PublishTimeout}
	if cfg.JetStream {
		js, err := jetstream.New(nc)
		if err != nil {
			return err
		}
		if cfg.JetStreamCreate {
			if err := retry(ctx, logger, "creating the JetStream stream", func() error {
				res, err := EnsureStream(ctx, js, cfg.JetStreamStream, cfg.SubjectPrefix, region, cfg.StreamSettings())
				if err != nil {
					return err
				}
				switch {
				case res.Created:
					logger.Info("created the JetStream stream", "stream", cfg.JetStreamStream)
				case len(res.Updated) > 0:
					logger.Info("updated the JetStream stream", "stream", cfg.JetStreamStream, "settings", strings.Join(res.Updated, ","))
				}
				if len(res.Kept) > 0 {
					logger.Warn("settings of the JetStream stream can't be changed after its creation; recreate the stream to change them",
						"stream", cfg.JetStreamStream, "settings", strings.Join(res.Kept, ","))
				}
				return nil
			}); err != nil {
				return nil // canceled
			}
		}
		pub = &JetStreamPublisher{JS: js, Timeout: cfg.PublishTimeout, Stream: cfg.JetStreamStream}
	}

	var types map[string]bool
	if len(cfg.EventTypes) > 0 {
		types = map[string]bool{}
		for _, t := range cfg.EventTypes {
			types[t] = true
		}
	}
	metrics := NewMetrics()
	bridge, err := New(NewAPISource(client, region, cfg.Namespace, cfg.Topics), pub, cp, Options{
		Region:             region,
		SubjectPrefix:      cfg.SubjectPrefix,
		StartAt:            cfg.StartAt,
		EventTypes:         types,
		Skip:               SkipVariables(cfg.StateNamespace, checkpointPath, leasePath),
		CheckpointInterval: cfg.CheckpointInterval,
	}, metrics, logger)
	if err != nil {
		return err
	}

	if cfg.MetricsAddr != "" {
		stop, err := serveMetrics(cfg.MetricsAddr, metrics, logger)
		if err != nil {
			return err
		}
		defer stop()
	}

	logger.Info("starting the NATS bridge", "region", region, "namespace", cfg.Namespace,
		"topics", topicsString(cfg.Topics), "subjects", cfg.SubjectPrefix+"."+subjectToken(region)+".<topic>",
		"jetstream", cfg.JetStream, "checkpoint", cfg.Checkpoint, "leader_election", cfg.LeaderElect, "identity", identity)
	if le != nil {
		bridge.RunElected(ctx, le)
		return nil
	}
	metrics.SetLeader(true)
	return bridge.Run(ctx)
}

// retry calls f with backoff until it succeeds (nil) or ctx is canceled
// (ctx.Err()).
func retry(ctx context.Context, logger *slog.Logger, what string, f func() error) error {
	delay := time.Second
	for {
		err := f()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		logger.Warn(what+" failed", "error", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay = min(delay*2, 30*time.Second)
	}
}

func connectNATS(cfg *Config, region string, logger *slog.Logger) (*nats.Conn, error) {
	name := cfg.NATSName
	if name == "" {
		name = "norca-natsbridge-" + region
	}
	opts := []nats.Option{
		nats.Name(name),
		nats.MaxReconnects(-1),
		nats.RetryOnFailedConnect(true),
		// No reconnect buffer: a publish while disconnected fails at once
		// and the batch is published again after the reconnect, instead of
		// being sent twice.
		nats.ReconnectBufSize(-1),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			// Called with a nil error when the connection is closed.
			if err != nil {
				logger.Warn("disconnected from NATS", "error", err)
			}
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			logger.Info("connected to NATS", "url", nc.ConnectedUrlRedacted())
		}),
	}
	if cfg.NATSCreds != "" {
		opts = append(opts, nats.UserCredentials(cfg.NATSCreds))
	}
	if cfg.NATSNKey != "" {
		opt, err := nats.NkeyOptionFromSeed(cfg.NATSNKey)
		if err != nil {
			return nil, fmt.Errorf("reading the NKey seed: %w", err)
		}
		opts = append(opts, opt)
	}
	if cfg.NATSToken != "" {
		opts = append(opts, nats.Token(cfg.NATSToken))
	}
	if cfg.NATSCA != "" {
		opts = append(opts, nats.RootCAs(cfg.NATSCA))
	}
	if cfg.NATSCert != "" {
		opts = append(opts, nats.ClientCert(cfg.NATSCert, cfg.NATSKey))
	}
	nc, err := nats.Connect(cfg.NATSURL, opts...)
	if err != nil {
		return nil, fmt.Errorf("connecting to NATS: %w", err)
	}
	if nc.IsConnected() {
		logger.Info("connected to NATS", "url", nc.ConnectedUrlRedacted())
	} else {
		logger.Warn("NATS is not reachable yet, retrying in the background", "url", redactNATSURL(cfg.NATSURL))
	}
	return nc, nil
}

// redactNATSURL redacts userinfo credentials in a (potentially comma-separated)
// NATS URL string before logging.
func redactNATSURL(raw string) string {
	parts := strings.Split(raw, ",")
	for i, p := range parts {
		trimmed := strings.TrimSpace(p)
		if u, err := url.Parse(trimmed); err == nil && u.User != nil {
			parts[i] = u.Redacted()
		}
	}
	return strings.Join(parts, ",")
}

// newAPIClient returns an API client configured like api.DefaultConfig,
// with addr and token overriding the environment. The address may be a unix
// socket ("unix:///path/to/api.sock"), as used to reach the task API socket
// of a task with its workload identity. It also returns the region of the
// configuration.
func newAPIClient(addr, token string) (*api.Client, string, error) {
	conf := api.DefaultConfig()
	if addr != "" {
		conf.Address = addr
	}
	if token != "" {
		conf.SecretID = token
	}
	if socket, ok := strings.CutPrefix(conf.Address, "unix://"); ok {
		// Not parsed as a URL: on Windows the path is a drive path.
		if socket == "" {
			return nil, "", errors.New("the unix socket address has no path")
		}
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		conf.Address = "http://127.0.0.1"
		conf.HttpClient = &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return dialer.DialContext(ctx, "unix", socket)
				},
				MaxIdleConnsPerHost: 8,
				IdleConnTimeout:     90 * time.Second,
			},
		}
	}
	client, err := api.NewClient(conf)
	return client, conf.Region, err
}

// serveMetrics serves /metrics and /healthz on addr until stop is called.
func serveMetrics(addr string, metrics *Metrics, logger *slog.Logger) (stop func(), err error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listening on the metrics address: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server failed", "error", err)
		}
	}()
	logger.Info("serving metrics", "addr", ln.Addr().String())
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}, nil
}
