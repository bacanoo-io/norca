// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package natsbridge

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/nats-io/nats.go"
)

// Inbound mode: NATS messages trigger syncs of the cluster pack registries
// through the Norca API. The cluster never depends on them: registries
// still sync on request and on their interval; a message only makes the
// sync come sooner.
//
//   - Push notices on <prefix>.packs.pushed[.>] (norca-natsbridge
//     notify-push, in the CI that pushes packs) sync the registries of
//     the source.
//   - Registry events of other regions on <prefix>.*.CustomObject sync the
//     registry of the same name and source when its packs differ.

// RegistryKindRef is the custom object kind of the cluster pack registries.
const RegistryKindRef = "packs.norca.io/Registry"

// PushSubject is the subject of push notices under the subject prefix. The
// region token "packs" is reserved for it.
const PushSubject = "packs.pushed"

// PushNotice announces that packs were pushed to a registry source.
type PushNotice struct {
	// Source is the source of the registry (oci://..., a Git URL, ...).
	Source string `json:"Source,omitempty"`
	// Registry names a cluster registry instead of, or besides, Source.
	Registry string `json:"Registry,omitempty"`

	Pack    string `json:"Pack,omitempty"`
	Version string `json:"Version,omitempty"`
	Digest  string `json:"Digest,omitempty"`
}

// Registry is the part of a cluster pack registry the syncer reads, in the
// JSON of the API and of the object events.
type Registry struct {
	Name string
	Spec struct {
		Type   string `json:"type"`
		Source string `json:"source"`
	}
	Status struct {
		Phase string `json:"phase"`
		Packs []struct {
			Name    string
			Version string
			Digest  string
		} `json:"packs"`
	}
}

// digests is the set of the pack versions and digests of the registry.
func (r *Registry) digests() []string {
	out := make([]string, 0, len(r.Status.Packs))
	for _, p := range r.Status.Packs {
		out = append(out, p.Name+"@"+p.Version+"="+p.Digest)
	}
	slices.Sort(out)
	return out
}

// RegistryAPI lists and syncs the cluster pack registries of a region.
type RegistryAPI interface {
	Registries(ctx context.Context) ([]*Registry, error)
	Sync(ctx context.Context, name string) error
}

// apiRegistries is the RegistryAPI of the Norca API client.
type apiRegistries struct {
	client *api.Client
	region string
}

func (a *apiRegistries) Registries(ctx context.Context) ([]*Registry, error) {
	var out []*Registry
	q := (&api.QueryOptions{Region: a.region}).WithContext(ctx)
	if _, err := a.client.Raw().Query("/v1/pack-registries", &out, q); err != nil {
		return nil, err
	}
	return out, nil
}

func (a *apiRegistries) Sync(ctx context.Context, name string) error {
	w := (&api.WriteOptions{Region: a.region}).WithContext(ctx)
	_, err := a.client.Raw().Write("/v1/pack-registry/"+url.PathEscape(name)+"/sync", nil, nil, w)
	return err
}

// Syncer turns push notices and registry events of other regions into
// registry syncs.
type Syncer struct {
	API    RegistryAPI
	Region string

	// Cooldown is the minimum time between two syncs of a registry that
	// the syncer triggers.
	Cooldown time.Duration

	Logger *slog.Logger

	mu   sync.Mutex
	last map[string]time.Time
	now  func() time.Time
}

// normalizeSource makes sources comparable: no scheme case, no trailing
// slash or ".git".
func normalizeSource(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "://"); i > 0 {
		s = strings.ToLower(s[:i]) + s[i:]
	}
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")
	return s
}

func (s *Syncer) logger() *slog.Logger {
	if s.Logger == nil {
		return slog.Default()
	}
	return s.Logger
}

// trigger syncs a registry unless the syncer synced it within the cooldown.
func (s *Syncer) trigger(ctx context.Context, name, reason string) bool {
	s.mu.Lock()
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	if s.last == nil {
		s.last = map[string]time.Time{}
	}
	if t, ok := s.last[name]; ok && now().Sub(t) < s.Cooldown {
		s.mu.Unlock()
		s.logger().Debug("registry sync skipped: cooldown", "registry", name, "reason", reason)
		return false
	}
	s.last[name] = now()
	s.mu.Unlock()

	if err := s.API.Sync(ctx, name); err != nil {
		s.logger().Warn("registry sync failed", "registry", name, "reason", reason, "error", err)
		return false
	}
	s.logger().Info("registry synced", "registry", name, "reason", reason)
	return true
}

// HandlePush syncs the registries a push notice names or whose source it
// names. It returns the synced registries.
func (s *Syncer) HandlePush(ctx context.Context, n *PushNotice) ([]string, error) {
	if n.Source == "" && n.Registry == "" {
		return nil, fmt.Errorf("the push notice names neither a source nor a registry")
	}
	regs, err := s.API.Registries(ctx)
	if err != nil {
		return nil, err
	}
	source := normalizeSource(n.Source)
	var synced []string
	for _, r := range regs {
		if (n.Registry != "" && r.Name == n.Registry) ||
			(source != "" && normalizeSource(r.Spec.Source) == source) {
			if s.trigger(ctx, r.Name, "push") {
				synced = append(synced, r.Name)
			}
		}
	}
	return synced, nil
}

// registryEvent is the part of an object event message the syncer reads.
type registryEvent struct {
	Region  string
	Type    string
	Payload struct {
		Object *struct {
			Group string
			Kind  string
			Registry
		}
	}
}

// HandleRegistryEvent syncs the local registry of the same name and source
// when the registry of another region found other packs. It returns
// whether it synced.
func (s *Syncer) HandleRegistryEvent(ctx context.Context, data []byte) (bool, error) {
	var ev registryEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return false, err
	}
	obj := ev.Payload.Object
	if ev.Region == "" || ev.Region == s.Region || obj == nil ||
		obj.Group+"/"+obj.Kind != RegistryKindRef || strings.HasSuffix(ev.Type, "Deleted") ||
		obj.Status.Phase != "Ready" {
		return false, nil
	}
	remote := &obj.Registry
	regs, err := s.API.Registries(ctx)
	if err != nil {
		return false, err
	}
	for _, local := range regs {
		if local.Name != remote.Name {
			continue
		}
		// The same name with another source is another registry: syncing
		// it would never converge.
		if local.Spec.Type != remote.Spec.Type || normalizeSource(local.Spec.Source) != normalizeSource(remote.Spec.Source) {
			return false, nil
		}
		if slices.Equal(local.digests(), remote.digests()) {
			return false, nil
		}
		return s.trigger(ctx, local.Name, "region "+ev.Region), nil
	}
	return false, nil
}

// Subscribe subscribes the syncer in a queue group of the region, so that
// one bridge instance of the region handles each message.
func (s *Syncer) Subscribe(ctx context.Context, nc *nats.Conn, prefix string, pushes, regions bool) ([]*nats.Subscription, error) {
	queue := "norca-natsbridge-sync." + subjectToken(s.Region)
	var subs []*nats.Subscription
	add := func(subject string, handle func(*nats.Msg)) error {
		sub, err := nc.QueueSubscribe(subject, queue, handle)
		if err != nil {
			return fmt.Errorf("subscribing to %s: %w", subject, err)
		}
		subs = append(subs, sub)
		return nil
	}
	if pushes {
		handle := func(m *nats.Msg) {
			var n PushNotice
			if err := json.Unmarshal(m.Data, &n); err != nil {
				s.logger().Warn("invalid push notice", "subject", m.Subject, "error", err)
				return
			}
			if _, err := s.HandlePush(ctx, &n); err != nil {
				s.logger().Warn("push notice not handled", "subject", m.Subject, "error", err)
			}
		}
		for _, subject := range []string{prefix + "." + PushSubject, prefix + "." + PushSubject + ".>"} {
			if err := add(subject, handle); err != nil {
				return subs, err
			}
		}
	}
	if regions {
		handle := func(m *nats.Msg) {
			if _, err := s.HandleRegistryEvent(ctx, m.Data); err != nil {
				s.logger().Warn("registry event not handled", "subject", m.Subject, "error", err)
			}
		}
		if err := add(prefix+".*.CustomObject", handle); err != nil {
			return subs, err
		}
	}
	return subs, nil
}

// PublishPush publishes a push notice.
func PublishPush(nc *nats.Conn, prefix string, n *PushNotice) error {
	if n.Source == "" && n.Registry == "" {
		return fmt.Errorf("a push notice needs -source or -registry")
	}
	b, err := json.Marshal(n)
	if err != nil {
		return err
	}
	if err := nc.Publish(prefix+"."+PushSubject, b); err != nil {
		return err
	}
	return nc.Flush()
}

// ParsePushConfig parses the arguments of the notify-push command: the NATS
// connection flags of the bridge, the subject prefix and the notice.
func ParsePushConfig(args []string, getenv func(string) string, output io.Writer) (*Config, *PushNotice, error) {
	envOr := func(def, name string) string {
		if v := getenv(name); v != "" {
			return v
		}
		return def
	}
	cfg := &Config{}
	n := &PushNotice{}
	fs := flag.NewFlagSet("norca-natsbridge notify-push", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&cfg.NATSURL, "nats-url", envOr("nats://127.0.0.1:4222", "NATS_URL"), "NATS server URLs, comma separated (env NATS_URL)")
	fs.StringVar(&cfg.NATSCreds, "nats-creds", getenv("NATS_CREDS"), "NATS credentials file (env NATS_CREDS)")
	fs.StringVar(&cfg.NATSNKey, "nats-nkey", getenv("NATS_NKEY"), "NATS NKey seed file (env NATS_NKEY)")
	fs.StringVar(&cfg.NATSToken, "nats-token", getenv("NATS_TOKEN"), "NATS authentication token (env NATS_TOKEN)")
	fs.StringVar(&cfg.NATSCA, "nats-ca", getenv("NATS_CA"), "CA certificate file for the NATS TLS connection (env NATS_CA)")
	fs.StringVar(&cfg.NATSCert, "nats-cert", getenv("NATS_CERT"), "client certificate file for NATS (env NATS_CERT)")
	fs.StringVar(&cfg.NATSKey, "nats-key", getenv("NATS_KEY"), "client key file for NATS (env NATS_KEY)")
	fs.StringVar(&cfg.SubjectPrefix, "subject-prefix", envOr("nomad", "NATSBRIDGE_SUBJECT_PREFIX"), "first part of the subject <prefix>.packs.pushed (env NATSBRIDGE_SUBJECT_PREFIX)")
	fs.StringVar(&n.Source, "source", "", "source of the registry the packs were pushed to, e.g. oci://ghcr.io/org/packs")
	fs.StringVar(&n.Registry, "registry", "", "name of a cluster registry to sync")
	fs.StringVar(&n.Pack, "pack", "", "pushed pack (informational)")
	fs.StringVar(&n.Version, "version", "", "pushed version (informational)")
	fs.StringVar(&n.Digest, "digest", "", "pushed digest (informational)")
	if err := fs.Parse(args); err != nil {
		return nil, nil, err
	}
	if fs.NArg() > 0 {
		return nil, nil, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if n.Source == "" && n.Registry == "" {
		return nil, nil, errors.New("-source or -registry is required")
	}
	if err := validPrefix(cfg.SubjectPrefix); err != nil {
		return nil, nil, err
	}
	if (cfg.NATSCert == "") != (cfg.NATSKey == "") {
		return nil, nil, errors.New("-nats-cert and -nats-key must be set together")
	}
	cfg.NATSName = "norca-natsbridge-notify-push"
	return cfg, n, nil
}

// NotifyPush publishes a push notice and waits until NATS received it.
func NotifyPush(cfg *Config, n *PushNotice, logger *slog.Logger) error {
	nc, err := connectNATS(cfg, "notify", logger)
	if err != nil {
		return err
	}
	defer nc.Close()
	if !nc.IsConnected() {
		deadline := time.Now().Add(10 * time.Second)
		for !nc.IsConnected() && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
		if !nc.IsConnected() {
			return fmt.Errorf("NATS is not reachable at %s", redactNATSURL(cfg.NATSURL))
		}
	}
	return PublishPush(nc, cfg.SubjectPrefix, n)
}
