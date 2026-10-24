// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/controller"
)

// Service meta keys that mark a service for a load balancer. They stand in
// for the planned jobspec block
//
//	service { expose { load_balancer { class = "..." port = 443 internal = false } } }
//
// until the jobspec gets it:
//
//	service {
//	  name = "api-public"
//	  port = "https"
//	  meta {
//	    norca_lb_class    = "nomad"
//	    norca_lb_port     = "443"
//	    norca_lb_internal = "false"
//	  }
//	}
//
// (Meta keys in HCL2 blocks must be identifiers, hence the underscores.)
const (
	markClass    = "norca_lb_class"
	markPort     = "norca_lb_port"
	markInternal = "norca_lb_internal"

	// labelManagedBy marks objects created from service marks; only those
	// are updated and deleted by the discovery controller.
	labelManagedBy  = "managed-by"
	managedByMarker = "service-mark"
)

// mark is a service marked for a load balancer.
type mark struct {
	JobID string
	Spec  Spec
}

// marks returns the marked services of a job, by service name.
func marks(job *api.Job) map[string]mark {
	out := map[string]mark{}
	if job == nil || job.ID == nil || (job.Stop != nil && *job.Stop) {
		return out
	}
	add := func(s *api.Service) {
		class, ok := s.Meta[markClass]
		if !ok || s.Name == "" || strings.Contains(s.Name, "${") {
			return
		}
		port, err := strconv.Atoi(s.Meta[markPort])
		if err != nil {
			port = 0 // reported by the validation of the object
		}
		internal, _ := strconv.ParseBool(s.Meta[markInternal])
		if _, dup := out[s.Name]; dup {
			return
		}
		out[s.Name] = mark{JobID: *job.ID, Spec: Spec{Service: s.Name, Class: class, Port: port, Internal: internal}}
	}
	for _, tg := range job.TaskGroups {
		for _, s := range tg.Services {
			add(s)
		}
		for _, t := range tg.Tasks {
			for _, s := range t.Services {
				add(s)
			}
		}
	}
	return out
}

// markIndex holds the marked services of the jobs, maintained by
// markSource and read by the discovery reconciler.
type markIndex struct {
	mu    sync.Mutex
	byJob map[string]map[string]mark // namespace/jobID -> service -> mark
}

func (m *markIndex) set(namespace, jobID string, ms map[string]mark) (changed []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := namespace + "/" + jobID
	old := m.byJob[key]
	for name := range old {
		changed = append(changed, name)
	}
	for name := range ms {
		if _, ok := old[name]; !ok {
			changed = append(changed, name)
		}
	}
	if len(ms) == 0 {
		delete(m.byJob, key)
	} else {
		m.byJob[key] = ms
	}
	return changed
}

// lookup returns the mark of a service; if several jobs mark it, the job
// with the lowest ID wins.
func (m *markIndex) lookup(namespace, service string) (mark, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := slices.Sorted(maps.Keys(m.byJob))
	for _, k := range keys {
		if !strings.HasPrefix(k, namespace+"/") {
			continue
		}
		if mk, ok := m.byJob[k][service]; ok {
			return mk, true
		}
	}
	return mark{}, false
}

// markSource watches the jobs and reconciles the objects of the services
// whose marks changed.
type markSource struct {
	index   *markIndex
	backend controller.Backend
	ns      string
}

func (s *markSource) Name() string { return "service-marks" }

func (s *markSource) Run(ctx context.Context, sc *controller.SourceContext) {
	topics := map[api.Topic][]string{api.TopicJob: {"*"}}
	sc.Watch(ctx, s.Name(), topics, func(ctx context.Context) (uint64, error) {
		stubs, index, err := s.backend.ListJobs(ctx, s.ns, 0)
		if err != nil {
			return 0, err
		}
		s.index.mu.Lock()
		s.index.byJob = map[string]map[string]mark{}
		s.index.mu.Unlock()
		for _, stub := range stubs {
			if stub.Stop {
				continue
			}
			job, err := s.backend.ReadJob(ctx, stub.Namespace, stub.ID)
			if errors.Is(err, controller.ErrNotFound) {
				continue
			} else if err != nil {
				return 0, err
			}
			for _, name := range s.index.set(stub.Namespace, stub.ID, marks(job)) {
				sc.Enqueue(controller.ObjectKey{Namespace: stub.Namespace, Name: name})
			}
		}
		// The Job topic is covered by the job listing, but the restored
		// base index of a server may be newer; the watch falls back to
		// the newest events after a 410 Gone.
		return index, nil
	}, func(_ context.Context, ev *api.Event) error {
		job, err := ev.Job()
		if err != nil || job == nil || job.ID == nil {
			return nil
		}
		ns := ev.Namespace
		if job.Namespace != nil {
			ns = *job.Namespace
		}
		for _, name := range s.index.set(ns, *job.ID, marks(job)) {
			sc.Enqueue(controller.ObjectKey{Namespace: ns, Name: name})
		}
		return nil
	})
}

// discoveryReconciler creates, updates and deletes LoadBalancer objects for
// marked services. Objects created by users (without the managed-by label)
// are never touched.
type discoveryReconciler struct {
	ctrl   *controller.Controller
	index  *markIndex
	logger *slog.Logger
}

func (r *discoveryReconciler) Reconcile(ctx context.Context, key controller.ObjectKey) (time.Duration, error) {
	validName := controller.ValidateName
	if r.ctrl.Objects().Storage() == controller.StorageNative {
		validName = controller.ValidateNativeName
	}
	if validName(key.Name) != nil {
		return 0, nil
	}
	mk, marked := r.index.lookup(key.Namespace, key.Name)
	obj, exists := r.ctrl.Get(key)
	managed := exists && obj.Labels[labelManagedBy] == managedByMarker

	switch {
	case !marked && managed:
		r.logger.Info("deleting load balancer of unmarked service", "object", key.String())
		err := r.ctrl.Objects().Delete(ctx, key, obj.ModifyIndex)
		if errors.Is(err, controller.ErrConflict) {
			return time.Second, nil
		}
		return 0, err
	case !marked:
		return 0, nil
	case exists && !managed:
		r.logger.Debug("load balancer exists and is managed by its user", "object", key.String())
		return 0, nil
	}

	specJSON, err := json.Marshal(mk.Spec)
	if err != nil {
		return 0, err
	}
	if !exists {
		r.logger.Info("creating load balancer for marked service", "object", key.String(), "job", mk.JobID)
		_, err := r.ctrl.Objects().Create(ctx, &controller.Object{
			Key:    key,
			Labels: map[string]string{labelManagedBy: managedByMarker, "job": mk.JobID},
			Spec:   specJSON,
		})
		if errors.Is(err, controller.ErrConflict) {
			return time.Second, nil
		}
		return 0, err
	}

	// Compared with the defaults filled in: native objects are stored with
	// the default values of the schema.
	var cur Spec
	_ = obj.DecodeSpec(&cur)
	cur.Defaults()
	want := mk.Spec
	want.Defaults()
	curJSON, _ := json.Marshal(cur)
	wantJSON, _ := json.Marshal(want)
	if string(curJSON) == string(wantJSON) && obj.Labels["job"] == mk.JobID {
		return 0, nil
	}
	r.logger.Info("updating load balancer of marked service", "object", key.String())
	next := obj.Copy()
	next.Spec = specJSON
	next.Labels["job"] = mk.JobID
	if _, err := r.ctrl.Objects().Update(ctx, next); err != nil {
		if errors.Is(err, controller.ErrConflict) {
			return time.Second, nil
		}
		return 0, fmt.Errorf("updating %s: %w", key, err)
	}
	return 0, nil
}
