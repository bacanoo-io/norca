// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package natsbridge

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// DefaultTopics are the topics forwarded by default. They are the topics a
// workload identity with read access to the jobs and nodes may subscribe
// to; topics that need a management token (ACL objects, node pools, "*")
// must be named explicitly.
const DefaultTopics = "Job,Deployment,Evaluation,Allocation,Node,Service,Scaling"

// Checkpoint stores.
const (
	CheckpointVariable = "variable"
	CheckpointFile     = "file"
	CheckpointNone     = "none"
)

// Config is the configuration of the bridge command. Every field can be set
// with a flag; most have an environment variable as default.
type Config struct {
	// Norca API. Empty Addr and Token use the defaults of the API client
	// (NORCA_ADDR, NORCA_TOKEN, ... with the NOMAD_ names as fallback). The
	// address may be the task API socket ("unix://${NOMAD_SECRETS_DIR}/api.sock").
	Addr  string
	Token string

	// Region of the event stream; defaults to the region of the API client
	// (NORCA_REGION / NOMAD_REGION, set in every task), else the region of
	// the agent.
	Region string

	// Events.
	Namespace  string
	Topics     []TopicFilter
	EventTypes []string
	StartAt    string

	// NATS.
	NATSURL   string
	NATSName  string
	NATSCreds string
	NATSNKey  string
	NATSToken string
	NATSCA    string
	NATSCert  string
	NATSKey   string

	SubjectPrefix  string
	PublishTimeout time.Duration

	JetStream           bool
	JetStreamStream     string
	JetStreamCreate     bool
	JetStreamDuplicates time.Duration

	// State: the checkpoint and the leader election lease.
	Checkpoint         string
	CheckpointPath     string
	CheckpointInterval time.Duration
	StateNamespace     string
	StatePrefix        string

	LeaderElect   bool
	LeasePath     string
	LeaseDuration time.Duration
	Identity      string

	MetricsAddr string
	LogLevel    string
}

// ParseConfig parses the command line arguments; getenv supplies the
// defaults from the environment.
func ParseConfig(args []string, getenv func(string) string, output io.Writer) (*Config, error) {
	env := func(names ...string) string {
		for _, n := range names {
			if v := getenv(n); v != "" {
				return v
			}
		}
		return ""
	}
	envOr := func(def string, names ...string) string {
		if v := env(names...); v != "" {
			return v
		}
		return def
	}
	var parseErrs []error
	envBool := func(def bool, name string) bool {
		v := getenv(name)
		if v == "" {
			return def
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			parseErrs = append(parseErrs, fmt.Errorf("%s: %w", name, err))
		}
		return b
	}
	envDuration := func(def time.Duration, name string) time.Duration {
		v := getenv(name)
		if v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			parseErrs = append(parseErrs, fmt.Errorf("%s: %w", name, err))
		}
		return d
	}

	cfg := &Config{}
	var topics, eventTypes string
	fs := flag.NewFlagSet("norca-natsbridge", flag.ContinueOnError)
	fs.SetOutput(output)

	fs.StringVar(&cfg.Addr, "addr", "", "Norca HTTP API address or unix:// task API socket (default: NORCA_ADDR)")
	fs.StringVar(&cfg.Token, "token", "", "Norca ACL token or workload identity (default: NORCA_TOKEN)")
	fs.StringVar(&cfg.Region, "region", "", "region of the event stream (default: NORCA_REGION / NOMAD_REGION, else the agent's region)")

	fs.StringVar(&cfg.Namespace, "namespace", envOr("*", "NATSBRIDGE_NAMESPACE"), `namespace of the events, "*" for all (env NATSBRIDGE_NAMESPACE)`)
	fs.StringVar(&topics, "topics", envOr(DefaultTopics, "NATSBRIDGE_TOPICS"), `comma separated topics, "Topic" or "Topic:key" (env NATSBRIDGE_TOPICS)`)
	fs.StringVar(&eventTypes, "event-types", env("NATSBRIDGE_EVENT_TYPES"), "comma separated event types to publish; empty publishes all (env NATSBRIDGE_EVENT_TYPES)")
	fs.StringVar(&cfg.StartAt, "start-at", envOr(StartNow, "NATSBRIDGE_START_AT"), `start position without a checkpoint: "now" or "oldest" (env NATSBRIDGE_START_AT)`)

	fs.StringVar(&cfg.NATSURL, "nats-url", envOr("nats://127.0.0.1:4222", "NATS_URL"), "NATS server URLs, comma separated (env NATS_URL)")
	fs.StringVar(&cfg.NATSName, "nats-name", "", "NATS connection name (default: norca-natsbridge-<region>)")
	fs.StringVar(&cfg.NATSCreds, "nats-creds", env("NATS_CREDS"), "NATS credentials file (env NATS_CREDS)")
	fs.StringVar(&cfg.NATSNKey, "nats-nkey", env("NATS_NKEY"), "NATS NKey seed file (env NATS_NKEY)")
	fs.StringVar(&cfg.NATSToken, "nats-token", env("NATS_TOKEN"), "NATS authentication token (env NATS_TOKEN)")
	fs.StringVar(&cfg.NATSCA, "nats-ca", env("NATS_CA"), "CA certificate file for the NATS TLS connection (env NATS_CA)")
	fs.StringVar(&cfg.NATSCert, "nats-cert", env("NATS_CERT"), "client certificate file for NATS (env NATS_CERT)")
	fs.StringVar(&cfg.NATSKey, "nats-key", env("NATS_KEY"), "client key file for NATS (env NATS_KEY)")

	fs.StringVar(&cfg.SubjectPrefix, "subject-prefix", envOr("nomad", "NATSBRIDGE_SUBJECT_PREFIX"), "first part of the subjects <prefix>.<region>.<topic> (env NATSBRIDGE_SUBJECT_PREFIX)")
	fs.DurationVar(&cfg.PublishTimeout, "publish-timeout", envDuration(10*time.Second, "NATSBRIDGE_PUBLISH_TIMEOUT"), "timeout of publishing one batch (env NATSBRIDGE_PUBLISH_TIMEOUT)")

	fs.BoolVar(&cfg.JetStream, "jetstream", envBool(false, "NATSBRIDGE_JETSTREAM"), "publish with JetStream and wait for the acknowledgements (env NATSBRIDGE_JETSTREAM)")
	fs.StringVar(&cfg.JetStreamStream, "jetstream-stream", env("NATSBRIDGE_JETSTREAM_STREAM"), "JetStream stream the messages must be stored in (env NATSBRIDGE_JETSTREAM_STREAM)")
	fs.BoolVar(&cfg.JetStreamCreate, "jetstream-create", envBool(false, "NATSBRIDGE_JETSTREAM_CREATE"), "create the -jetstream-stream stream for <prefix>.<region>.> if it does not exist (env NATSBRIDGE_JETSTREAM_CREATE)")
	fs.DurationVar(&cfg.JetStreamDuplicates, "jetstream-duplicates", envDuration(10*time.Minute, "NATSBRIDGE_JETSTREAM_DUPLICATES"), "duplicate window of a stream created with -jetstream-create")

	fs.StringVar(&cfg.Checkpoint, "checkpoint", envOr(CheckpointVariable, "NATSBRIDGE_CHECKPOINT"), `checkpoint store: "variable", "file" or "none" (env NATSBRIDGE_CHECKPOINT)`)
	fs.StringVar(&cfg.CheckpointPath, "checkpoint-path", env("NATSBRIDGE_CHECKPOINT_PATH"), "Variable path or file of the checkpoint (default: <state-prefix>/checkpoint; required for \"file\")")
	fs.DurationVar(&cfg.CheckpointInterval, "checkpoint-interval", envDuration(time.Second, "NATSBRIDGE_CHECKPOINT_INTERVAL"), "minimum time between checkpoint writes")
	fs.StringVar(&cfg.StateNamespace, "state-namespace", envOr("default", "NORCA_NAMESPACE", "NOMAD_NAMESPACE"), "namespace of the checkpoint and lease Variables (default: the job's namespace)")
	fs.StringVar(&cfg.StatePrefix, "state-prefix", "", "Variable path prefix of the checkpoint and the lease (default: nomad/jobs/<NOMAD_JOB_ID>, or natsbridge/<region> outside a job)")

	fs.BoolVar(&cfg.LeaderElect, "leader-elect", envBool(true, "NATSBRIDGE_LEADER_ELECT"), "elect one publishing instance over a Variables lease (env NATSBRIDGE_LEADER_ELECT)")
	fs.StringVar(&cfg.LeasePath, "lease-path", "", "Variable path of the lease (default: <state-prefix>/lease)")
	fs.DurationVar(&cfg.LeaseDuration, "lease-duration", 20*time.Second, "time after which a lease that is not renewed is taken over")
	fs.StringVar(&cfg.Identity, "identity", "", "identity of this instance (default: NOMAD_ALLOC_ID)")

	fs.StringVar(&cfg.MetricsAddr, "metrics-addr", envOr("127.0.0.1:9464", "NATSBRIDGE_METRICS_ADDR"), "listen address of /metrics and /healthz; empty disables (env NATSBRIDGE_METRICS_ADDR)")
	fs.StringVar(&cfg.LogLevel, "log-level", envOr("info", "NATSBRIDGE_LOG_LEVEL"), "log level: debug, info, warn or error")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if len(parseErrs) > 0 {
		return nil, errors.Join(parseErrs...)
	}

	var err error
	if cfg.Topics, err = ParseTopics(topics); err != nil {
		return nil, err
	}
	for _, t := range strings.Split(eventTypes, ",") {
		if t = strings.TrimSpace(t); t != "" {
			cfg.EventTypes = append(cfg.EventTypes, t)
		}
	}
	if cfg.StatePrefix == "" {
		if job := env("NORCA_JOB_ID", "NOMAD_JOB_ID"); job != "" {
			cfg.StatePrefix = "nomad/jobs/" + job
		}
	}
	return cfg, cfg.validate()
}

func (cfg *Config) validate() error {
	if err := validPrefix(cfg.SubjectPrefix); err != nil {
		return err
	}
	switch cfg.StartAt {
	case StartNow, StartOldest:
	default:
		return fmt.Errorf("invalid -start-at %q", cfg.StartAt)
	}
	switch cfg.Checkpoint {
	case CheckpointVariable, CheckpointNone:
	case CheckpointFile:
		if cfg.CheckpointPath == "" {
			return errors.New(`-checkpoint=file requires -checkpoint-path`)
		}
	default:
		return fmt.Errorf("invalid -checkpoint %q", cfg.Checkpoint)
	}
	if cfg.JetStreamCreate && cfg.JetStreamStream == "" {
		return errors.New("-jetstream-create requires -jetstream-stream")
	}
	if cfg.JetStreamStream != "" && !cfg.JetStream {
		return errors.New("-jetstream-stream requires -jetstream")
	}
	if (cfg.NATSCert == "") != (cfg.NATSKey == "") {
		return errors.New("-nats-cert and -nats-key must be set together")
	}
	if cfg.LeaseDuration < 2*time.Second {
		return errors.New("-lease-duration must be at least 2s")
	}
	return nil
}
