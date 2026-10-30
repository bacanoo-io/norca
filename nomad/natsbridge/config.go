// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package natsbridge

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
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
	// JetStreamReplicas is the number of replicas of a stream created with
	// JetStreamCreate; 3 keeps the events when a server (or the
	// datacenter it runs in) fails.
	JetStreamReplicas int
	// The limits and options of the stream created with JetStreamCreate;
	// see StreamSettings.
	JetStreamMaxAge      time.Duration
	JetStreamMaxBytes    int64
	JetStreamStorage     string
	JetStreamCompression string
	JetStreamAllowDirect bool
	JetStreamDenyDelete  bool
	JetStreamDenyPurge   bool

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

	// Inbound mode: NATS messages trigger syncs of the cluster pack
	// registries (sync.go).
	SyncPushes   bool
	SyncRegions  bool
	SyncCooldown time.Duration
	// SyncStream is the JetStream stream of the push notices; with
	// JetStream the inbound mode reads through durable consumers.
	SyncStream string
}

// registryTopic is the topic filter of the pack registry events that
// -sync-regions forwards to the other regions.
var registryTopic = TopicFilter{Topic: "CustomObject", Keys: []string{RegistryKindRef}}

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
	envInt := func(def int, name string) int {
		v := getenv(name)
		if v == "" {
			return def
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			parseErrs = append(parseErrs, fmt.Errorf("%s: %w", name, err))
		}
		return n
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
	fs.IntVar(&cfg.JetStreamReplicas, "jetstream-replicas", envInt(1, "NATSBRIDGE_JETSTREAM_REPLICAS"), "replicas of a stream created with -jetstream-create, 1 to 5 (env NATSBRIDGE_JETSTREAM_REPLICAS)")
	fs.DurationVar(&cfg.JetStreamMaxAge, "jetstream-max-age", envDuration(0, "NATSBRIDGE_JETSTREAM_MAX_AGE"), "maximum age of the messages of the stream, 0 for no limit (env NATSBRIDGE_JETSTREAM_MAX_AGE)")
	maxBytes := envOr("0", "NATSBRIDGE_JETSTREAM_MAX_BYTES")
	fs.StringVar(&maxBytes, "jetstream-max-bytes", maxBytes, `maximum size of the stream (of each replica), e.g. "512MB"; 0 for no limit (env NATSBRIDGE_JETSTREAM_MAX_BYTES)`)
	fs.StringVar(&cfg.JetStreamStorage, "jetstream-storage", envOr(StorageFile, "NATSBRIDGE_JETSTREAM_STORAGE"), `storage of a stream created with -jetstream-create: "file" or "memory" (env NATSBRIDGE_JETSTREAM_STORAGE)`)
	fs.StringVar(&cfg.JetStreamCompression, "jetstream-compression", envOr(CompressionNone, "NATSBRIDGE_JETSTREAM_COMPRESSION"), `compression of the stored messages: "none" or "s2" (env NATSBRIDGE_JETSTREAM_COMPRESSION)`)
	fs.BoolVar(&cfg.JetStreamAllowDirect, "jetstream-allow-direct", envBool(false, "NATSBRIDGE_JETSTREAM_ALLOW_DIRECT"), "let every replica answer direct gets, not only the leader (env NATSBRIDGE_JETSTREAM_ALLOW_DIRECT)")
	fs.BoolVar(&cfg.JetStreamDenyDelete, "jetstream-deny-delete", envBool(false, "NATSBRIDGE_JETSTREAM_DENY_DELETE"), "forbid deleting single messages of a stream created with -jetstream-create (env NATSBRIDGE_JETSTREAM_DENY_DELETE)")
	fs.BoolVar(&cfg.JetStreamDenyPurge, "jetstream-deny-purge", envBool(false, "NATSBRIDGE_JETSTREAM_DENY_PURGE"), "forbid purging a stream created with -jetstream-create (env NATSBRIDGE_JETSTREAM_DENY_PURGE)")

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

	fs.BoolVar(&cfg.SyncPushes, "sync-pushes", envBool(false, "NATSBRIDGE_SYNC_PUSHES"), "sync the pack registries of the sources named by push notices on <prefix>.packs.pushed (env NATSBRIDGE_SYNC_PUSHES)")
	fs.BoolVar(&cfg.SyncRegions, "sync-regions", envBool(false, "NATSBRIDGE_SYNC_REGIONS"), "publish the pack registry events and sync a registry when the one of the same name and source in another region found other packs (env NATSBRIDGE_SYNC_REGIONS)")
	fs.StringVar(&cfg.SyncStream, "sync-stream", envOr(DefaultPushStream, "NATSBRIDGE_SYNC_STREAM"), "with -jetstream: stream of the push notices, consumed durably (created with -jetstream-create) (env NATSBRIDGE_SYNC_STREAM)")
	fs.DurationVar(&cfg.SyncCooldown, "sync-cooldown", envDuration(time.Minute, "NATSBRIDGE_SYNC_COOLDOWN"), "minimum time between two syncs of a registry triggered from NATS (env NATSBRIDGE_SYNC_COOLDOWN)")

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
	if cfg.JetStreamMaxBytes, err = ParseBytes(maxBytes); err != nil {
		return nil, fmt.Errorf("-jetstream-max-bytes: %w", err)
	}
	if cfg.Topics, err = ParseTopics(topics); err != nil {
		return nil, err
	}
	if cfg.SyncRegions {
		cfg.Topics = withTopic(cfg.Topics, registryTopic)
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

// withTopic adds the keys of t to its topic in filters, unless the topic is
// forwarded without keys (all of its events).
func withTopic(filters []TopicFilter, t TopicFilter) []TopicFilter {
	for i, f := range filters {
		if f.Topic != t.Topic && f.Topic != "*" {
			continue
		}
		if len(f.Keys) == 0 {
			return filters
		}
		if f.Topic == t.Topic {
			for _, k := range t.Keys {
				if !slices.Contains(f.Keys, k) {
					filters[i].Keys = append(filters[i].Keys, k)
				}
			}
			return filters
		}
	}
	return append(filters, t)
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
	if cfg.JetStreamReplicas < 1 || cfg.JetStreamReplicas > 5 {
		return fmt.Errorf("-jetstream-replicas %d must be 1 to 5", cfg.JetStreamReplicas)
	}
	if cfg.JetStreamMaxAge < 0 {
		return errors.New("-jetstream-max-age can't be negative")
	}
	if cfg.JetStreamMaxAge > 0 && cfg.JetStreamMaxAge < cfg.JetStreamDuplicates {
		return fmt.Errorf("-jetstream-max-age %s must be at least the duplicate window -jetstream-duplicates %s", cfg.JetStreamMaxAge, cfg.JetStreamDuplicates)
	}
	switch cfg.JetStreamStorage {
	case StorageFile, StorageMemory:
	default:
		return fmt.Errorf(`invalid -jetstream-storage %q: "file" or "memory"`, cfg.JetStreamStorage)
	}
	switch cfg.JetStreamCompression {
	case CompressionNone, CompressionS2:
	default:
		return fmt.Errorf(`invalid -jetstream-compression %q: "none" or "s2"`, cfg.JetStreamCompression)
	}
	if cfg.JetStreamCompression == CompressionS2 && cfg.JetStreamStorage == StorageMemory {
		return errors.New("-jetstream-compression=s2 needs -jetstream-storage=file")
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
	if cfg.SyncPushes && cfg.JetStream && cfg.SyncStream == "" {
		return errors.New("-sync-pushes with -jetstream requires -sync-stream")
	}
	if cfg.SyncCooldown < 0 {
		return errors.New("-sync-cooldown can't be negative")
	}
	return nil
}
