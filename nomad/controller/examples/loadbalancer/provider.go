// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/controller"
)

// Provider implements load balancers of one class. Cloud providers (AWS
// NLB, Google Cloud Load Balancing, Hetzner) and a MetalLB-like provider
// plug in here; this prototype ships the self-hosted "nomad" provider.
type Provider interface {
	// Class is the name used in the spec.
	Class() string

	// Ensure creates or updates the load balancer of obj so that it
	// forwards to targets. It must be idempotent.
	Ensure(ctx context.Context, obj *controller.Object, spec Spec, targets []Target) (Result, error)

	// Delete removes everything the provider created for the object. The
	// object may no longer exist. Deleting nothing is not an error.
	Delete(ctx context.Context, key controller.ObjectKey) error
}

// Result is the state of a load balancer reported by a provider.
type Result struct {
	// Addresses are the frontend addresses.
	Addresses []string

	// Ready is true when the load balancer serves traffic as specified.
	Ready bool

	// Message explains a state that is not ready.
	Message string

	// Resource identifies what the provider created.
	Resource string
}

// Meta keys of jobs and Variables created by the nomad provider.
const (
	metaSpecHash = "norca.lb.spec-hash"
	itemTargets  = "targets"
	itemOwner    = "owner"
	proxyPort    = "lb"
)

// NomadProviderConfig configures the "nomad" provider, which runs the load
// balancer as a Nomad job: a small TCP proxy (the "proxy" subcommand of this
// binary) whose target list is rendered from the job's Variable
// (nomad/jobs/<job>), which the controller keeps up to date. Target changes
// therefore do not update the job.
type NomadProviderConfig struct {
	// JobPrefix is prepended to the object name to form the job ID.
	JobPrefix string

	// Driver of the proxy task: raw_exec, exec or docker.
	Driver string

	// Command is the path of this binary on the nodes (raw_exec, exec).
	Command string

	// Image is the container image with this binary (docker).
	Image string

	CPU      int
	MemoryMB int

	// ManagedBy is the identity the proxy jobs are protected for (see
	// controller.SetManagedBy); empty leaves them unprotected.
	ManagedBy string
}

type nomadProvider struct {
	cfg     NomadProviderConfig
	backend controller.Backend
	cluster Cluster
}

func newNomadProvider(cfg NomadProviderConfig, backend controller.Backend, cluster Cluster) (*nomadProvider, error) {
	if cfg.JobPrefix == "" {
		cfg.JobPrefix = "lb-"
	}
	if cfg.Driver == "" {
		cfg.Driver = "raw_exec"
	}
	switch cfg.Driver {
	case "raw_exec", "exec":
		if cfg.Command == "" {
			return nil, fmt.Errorf("the %s driver requires the path of the proxy command", cfg.Driver)
		}
	case "docker":
		if cfg.Image == "" {
			return nil, errors.New("the docker driver requires the proxy image")
		}
	default:
		return nil, fmt.Errorf("unsupported proxy driver %q", cfg.Driver)
	}
	if cfg.CPU <= 0 {
		cfg.CPU = 50
	}
	if cfg.MemoryMB <= 0 {
		cfg.MemoryMB = 32
	}
	return &nomadProvider{cfg: cfg, backend: backend, cluster: cluster}, nil
}

func (p *nomadProvider) Class() string { return "nomad" }

func (p *nomadProvider) jobID(name string) string { return p.cfg.JobPrefix + name }

// targetsPath is the job's own Variable, which its tasks can read with
// their workload identity without further ACL rules.
func targetsPath(jobID string) string { return "nomad/jobs/" + jobID }

func (p *nomadProvider) Ensure(ctx context.Context, obj *controller.Object, spec Spec, targets []Target) (Result, error) {
	jobID := p.jobID(obj.Key.Name)
	ns := obj.Key.Namespace
	res := Result{Resource: jobID}

	existing, err := p.backend.ReadJob(ctx, ns, jobID)
	if err != nil && !errors.Is(err, controller.ErrNotFound) {
		return res, err
	}
	if existing != nil && !controller.IsOwnedBy(existing.Meta, obj) {
		res.Message = fmt.Sprintf("job %q exists and is not owned by this load balancer", jobID)
		return res, nil
	}

	if err := p.writeTargets(ctx, obj, jobID, targets); err != nil {
		return res, err
	}

	job := p.desiredJob(obj, spec, jobID)
	stopped := existing != nil && existing.Stop != nil && *existing.Stop
	if existing == nil || stopped || existing.Meta[metaSpecHash] != job.Meta[metaSpecHash] {
		if err := p.backend.RegisterJob(ctx, job); err != nil {
			return res, fmt.Errorf("registering job %q: %w", jobID, err)
		}
	}

	res.Addresses, err = p.cluster.JobEndpoints(ctx, ns, jobID, proxyPort)
	if err != nil {
		return res, err
	}
	res.Ready = len(res.Addresses) >= spec.Replicas
	if !res.Ready {
		res.Message = fmt.Sprintf("%d of %d proxy instances running", len(res.Addresses), spec.Replicas)
	}
	return res, nil
}

func (p *nomadProvider) writeTargets(ctx context.Context, obj *controller.Object, jobID string, targets []Target) error {
	addrs := make([]string, len(targets))
	for i, t := range targets {
		addrs[i] = t.String()
	}
	want := strings.Join(addrs, "\n")
	path := targetsPath(jobID)

	cur, err := p.backend.ReadVariable(ctx, obj.Key.Namespace, path)
	switch {
	case errors.Is(err, controller.ErrNotFound):
		cur = &api.Variable{Namespace: obj.Key.Namespace, Path: path}
	case err != nil:
		return err
	case cur.Items[itemOwner] != controller.OwnerRef(obj):
		return fmt.Errorf("variable %q exists and is not owned by this load balancer", path)
	case cur.Items[itemTargets] == want:
		return nil
	}
	cur.Items = api.VariableItems{itemTargets: want, itemOwner: controller.OwnerRef(obj)}
	_, err = p.backend.WriteVariable(ctx, cur)
	return err
}

func (p *nomadProvider) desiredJob(obj *controller.Object, spec Spec, jobID string) *api.Job {
	ns := obj.Key.Namespace
	listen := ":${NOMAD_PORT_lb}"
	if spec.Internal {
		listen = "${NOMAD_ADDR_lb}"
	}
	args := []string{"proxy", "-listen", listen, "-targets", "${NOMAD_TASK_DIR}/targets"}

	var config map[string]any
	switch p.cfg.Driver {
	case "docker":
		config = map[string]any{"image": p.cfg.Image, "args": args, "ports": []string{proxyPort}}
	default:
		config = map[string]any{"command": p.cfg.Command, "args": args}
	}

	tmpl := fmt.Sprintf(`{{ with nomadVar %q }}{{ .%s }}{{ end }}`, targetsPath(jobID), itemTargets)
	count := spec.Replicas
	job := &api.Job{
		ID:          &jobID,
		Name:        &jobID,
		Namespace:   &ns,
		Type:        pointerOf("service"),
		Datacenters: spec.Datacenters,
		Meta:        map[string]string{},
		TaskGroups: []*api.TaskGroup{{
			Name:  pointerOf("proxy"),
			Count: &count,
			Networks: []*api.NetworkResource{{
				ReservedPorts: []api.Port{{Label: proxyPort, Value: spec.Port}},
			}},
			Tasks: []*api.Task{{
				Name:   "proxy",
				Driver: p.cfg.Driver,
				Config: config,
				Templates: []*api.Template{{
					EmbeddedTmpl: &tmpl,
					DestPath:     pointerOf("local/targets"),
					ChangeMode:   pointerOf("noop"),
				}},
				Resources: &api.Resources{CPU: &p.cfg.CPU, MemoryMB: &p.cfg.MemoryMB},
			}},
		}},
	}
	controller.SetOwner(job, obj)
	controller.SetManagedBy(job, p.cfg.ManagedBy)
	job.Meta[metaSpecHash] = hashJob(job)
	return job
}

func hashJob(job *api.Job) string {
	b, _ := json.Marshal(job)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func (p *nomadProvider) Delete(ctx context.Context, key controller.ObjectKey) error {
	owner := &controller.Object{Kind: Kind, Key: key}
	jobs, err := controller.ListOwnedJobs(ctx, p.backend, key.Namespace, owner)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if err := p.backend.DeregisterJob(ctx, j.Namespace, j.ID, true); err != nil {
			return fmt.Errorf("deregistering job %q: %w", j.ID, err)
		}
	}
	path := targetsPath(p.jobID(key.Name))
	v, err := p.backend.ReadVariable(ctx, key.Namespace, path)
	if errors.Is(err, controller.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	if v.Items[itemOwner] != controller.OwnerRef(owner) {
		return nil
	}
	return p.backend.DeleteVariable(ctx, key.Namespace, path, v.ModifyIndex)
}

func pointerOf[T any](v T) *T { return &v }
