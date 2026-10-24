// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/controller"
	"github.com/hashicorp/nomad/controller/fake"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// fakeCluster serves service targets and job endpoints from maps.
type fakeCluster struct {
	mu        sync.Mutex
	targets   map[string][]Target // namespace/service
	endpoints map[string][]string // namespace/job
}

func newFakeCluster() *fakeCluster {
	return &fakeCluster{targets: map[string][]Target{}, endpoints: map[string][]string{}}
}

func (c *fakeCluster) setTargets(ns, service string, ts ...Target) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.targets[ns+"/"+service] = ts
}

func (c *fakeCluster) setEndpoints(ns, job string, eps ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.endpoints[ns+"/"+job] = eps
}

func (c *fakeCluster) ServiceTargets(_ context.Context, ns, service string) ([]Target, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.targets[ns+"/"+service], nil
}

func (c *fakeCluster) JobEndpoints(_ context.Context, ns, job, _ string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.endpoints[ns+"/"+job], nil
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func waitFor(t *testing.T, desc string, fn func() bool) {
	t.Helper()
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(fn), wait.Timeout(5*time.Second), wait.Gap(10*time.Millisecond)),
		must.Sprint(desc))
}

type env struct {
	b       *fake.Backend
	cluster *fakeCluster
}

func startOperator(t *testing.T, discover bool) *env {
	return startOperatorWith(t, discover, controller.StorageVariables)
}

func startOperatorWith(t *testing.T, discover bool, storage controller.Storage) *env {
	t.Helper()
	b := fake.New()
	cluster := newFakeCluster()
	prov, err := newNomadProvider(NomadProviderConfig{Command: "/usr/local/bin/loadbalancer"}, b, cluster)
	must.NoError(t, err)

	r := &reconciler{
		cluster:         cluster,
		providers:       map[string]Provider{"nomad": prov},
		logger:          testLogger(),
		now:             time.Now,
		requeueNotReady: 50 * time.Millisecond,
		requeueReady:    time.Minute,
	}
	c, err := controller.New(b, r, controller.Options{
		Name:        "loadbalancer",
		Storage:     storage,
		Kind:        Kind,
		OwnsJobs:    true,
		Sources:     []controller.Source{serviceSource()},
		Logger:      testLogger(),
		RateLimiter: controller.NewExponentialBackoff[controller.ObjectKey](10*time.Millisecond, 100*time.Millisecond),
	})
	must.NoError(t, err)
	r.ctrl = c
	ctrls := []*controller.Controller{c}

	if discover {
		index := &markIndex{byJob: map[string]map[string]mark{}}
		d := &discoveryReconciler{index: index, logger: testLogger()}
		dc, err := controller.New(b, d, controller.Options{
			Name:      "loadbalancer-discovery",
			Storage:   storage,
			Kind:      Kind,
			Sources:   []controller.Source{&markSource{index: index, backend: b, ns: "default"}},
			Predicate: controller.AnyChange,
			Logger:    testLogger(),
		})
		must.NoError(t, err)
		d.ctrl = dc
		ctrls = append(ctrls, dc)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for _, c := range ctrls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.Run(ctx)
		}()
	}
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	return &env{b: b, cluster: cluster}
}

func (e *env) putLB(name, spec string) {
	e.b.PutVariable(&api.Variable{Path: Kind.Path(name), Items: api.VariableItems{"spec": spec}})
}

func (e *env) status(t *testing.T, name string) Status {
	v := e.b.GetVariable("default", Kind.Path(name))
	if v == nil {
		return Status{}
	}
	obj, err := controller.ObjectFromVariable(Kind, v)
	must.NoError(t, err)
	var s Status
	must.NoError(t, obj.DecodeStatus(&s))
	return s
}

func (e *env) targetsVar(job string) string {
	v := e.b.GetVariable("default", "nomad/jobs/"+job)
	if v == nil {
		return "<missing>"
	}
	return v.Items["targets"]
}

func TestLoadBalancer_Lifecycle(t *testing.T) {
	e := startOperator(t, false)
	e.cluster.setTargets("default", "web", Target{Address: "10.0.0.1", Port: 8001}, Target{Address: "10.0.0.2", Port: 8002})
	e.putLB("web", `{"service":"web","port":8080}`)

	// The proxy job is created, owned by the object, with the targets in
	// its Variable.
	waitFor(t, "job", func() bool { return e.b.GetJob("default", "lb-web") != nil })
	job := e.b.GetJob("default", "lb-web")
	must.Eq(t, "default/crd/norca-io/LoadBalancer/web", job.Meta[controller.OwnerMetaKey])
	must.Eq(t, 8080, job.TaskGroups[0].Networks[0].ReservedPorts[0].Value)
	must.Eq(t, "raw_exec", job.TaskGroups[0].Tasks[0].Driver)
	must.Eq(t, "10.0.0.1:8001\n10.0.0.2:8002", e.targetsVar("lb-web"))

	// Not ready until the proxy runs.
	waitFor(t, "progressing", func() bool { return e.status(t, "web").IsTrue(controller.ConditionProgressing) })
	must.False(t, e.status(t, "web").IsTrue(controller.ConditionReady))
	e.cluster.setEndpoints("default", "lb-web", "192.168.1.10:8080")
	waitFor(t, "ready", func() bool { return e.status(t, "web").IsTrue(controller.ConditionReady) })
	st := e.status(t, "web")
	must.Eq(t, []string{"192.168.1.10:8080"}, st.Addresses)
	must.Eq(t, []string{"10.0.0.1:8001", "10.0.0.2:8002"}, st.Targets)
	must.Eq(t, "lb-web", st.Resource)
	must.NotEq(t, "", st.ObservedSpecHash)

	// A target change updates the Variable, not the job.
	registers := e.b.Calls("RegisterJob")
	e.cluster.setTargets("default", "web", Target{Address: "10.0.0.3", Port: 8003})
	e.b.Publish(api.Event{Topic: api.TopicService, Type: "ServiceRegistration", Key: "svc-3", Namespace: "default",
		Payload: map[string]any{"Service": map[string]any{"ServiceName": "web", "Namespace": "default"}}})
	waitFor(t, "targets updated", func() bool { return e.targetsVar("lb-web") == "10.0.0.3:8003" })
	waitFor(t, "status targets", func() bool { return len(e.status(t, "web").Targets) == 1 })
	must.Eq(t, registers, e.b.Calls("RegisterJob"))

	// No targets: degraded.
	e.cluster.setTargets("default", "web")
	e.b.Publish(api.Event{Topic: api.TopicService, Type: "ServiceDeregistration", Key: "svc-3", Namespace: "default",
		Payload: map[string]any{"Service": map[string]any{"ServiceName": "web", "Namespace": "default"}}})
	waitFor(t, "degraded", func() bool { return e.status(t, "web").IsTrue(controller.ConditionDegraded) })
	must.Eq(t, "", e.targetsVar("lb-web"))

	// A spec change updates the job.
	e.putLB("web", `{"service":"web","port":9090,"replicas":2}`)
	waitFor(t, "job updated", func() bool {
		j := e.b.GetJob("default", "lb-web")
		return j.TaskGroups[0].Networks[0].ReservedPorts[0].Value == 9090 && *j.TaskGroups[0].Count == 2
	})

	// Someone purges the job: it is recreated.
	e.b.RemoveJob("default", "lb-web", true)
	waitFor(t, "job recreated", func() bool { return e.b.GetJob("default", "lb-web") != nil })

	// Deleting the object deletes the job and its Variable.
	e.b.RemoveVariable("default", Kind.Path("web"))
	waitFor(t, "job deleted", func() bool { return e.b.GetJob("default", "lb-web") == nil })
	waitFor(t, "variable deleted", func() bool { return e.targetsVar("lb-web") == "<missing>" })
}

func TestLoadBalancer_InvalidSpecAndForeignJob(t *testing.T) {
	e := startOperator(t, false)

	e.putLB("bad", `{"service":"","port":0}`)
	waitFor(t, "invalid", func() bool {
		c := e.status(t, "bad").Condition(controller.ConditionReady)
		return c != nil && c.Reason == "InvalidSpec"
	})
	e.putLB("odd", `{"service":"web","port":80,"class":"aws-nlb"}`)
	waitFor(t, "unknown class", func() bool {
		c := e.status(t, "odd").Condition(controller.ConditionReady)
		return c != nil && c.Reason == "UnknownClass"
	})

	// A job with the proxy's ID that the object does not own is left alone.
	foreign := api.NewServiceJob("lb-mine", "lb-mine", "global", 50)
	foreign.AddTaskGroup(api.NewTaskGroup("g", 1))
	e.b.PutJob(foreign)
	e.putLB("mine", `{"service":"web","port":81}`)
	waitFor(t, "not owned", func() bool {
		c := e.status(t, "mine").Condition(controller.ConditionProgressing)
		return c != nil && c.Status == controller.ConditionTrue && c.Message != ""
	})
	must.Eq(t, "", e.b.GetJob("default", "lb-mine").Meta[controller.OwnerMetaKey])
	e.b.RemoveVariable("default", Kind.Path("mine"))
	time.Sleep(200 * time.Millisecond)
	must.NotNil(t, e.b.GetJob("default", "lb-mine"))
}

func markedJob(id, service, class, port string) *api.Job {
	job := api.NewServiceJob(id, id, "global", 50)
	tg := api.NewTaskGroup("app", 1)
	tg.Services = []*api.Service{{Name: service, Provider: "nomad", Meta: map[string]string{
		markClass: class, markPort: port,
	}}}
	job.AddTaskGroup(tg)
	return job
}

func TestLoadBalancer_Discovery(t *testing.T) {
	e := startOperator(t, true)
	e.cluster.setTargets("default", "api-public", Target{Address: "10.0.0.9", Port: 443})

	// A user-managed object of the same name as a later mark is kept.
	e.putLB("other", `{"service":"other","port":82}`)

	e.b.PutJob(markedJob("api", "api-public", "nomad", "8443"))
	waitFor(t, "object created from the mark", func() bool {
		return e.b.GetVariable("default", Kind.Path("api-public")) != nil
	})
	obj, err := controller.ObjectFromVariable(Kind, e.b.GetVariable("default", Kind.Path("api-public")))
	must.NoError(t, err)
	must.Eq(t, managedByMarker, obj.Labels[labelManagedBy])
	var spec Spec
	must.NoError(t, obj.DecodeSpec(&spec))
	must.Eq(t, Spec{Service: "api-public", Class: "nomad", Port: 8443}, spec)

	// ... and the load balancer controller creates its proxy job.
	waitFor(t, "proxy job", func() bool { return e.b.GetJob("default", "lb-api-public") != nil })

	// Changing the mark updates the object.
	e.b.PutJob(markedJob("api", "api-public", "nomad", "9443"))
	waitFor(t, "object updated", func() bool {
		o, _ := controller.ObjectFromVariable(Kind, e.b.GetVariable("default", Kind.Path("api-public")))
		var s Spec
		_ = o.DecodeSpec(&s)
		return s.Port == 9443
	})

	// The user's object with a marked service name stays user-managed.
	e.b.PutJob(markedJob("other-job", "other", "nomad", "1234"))
	time.Sleep(200 * time.Millisecond)
	o, _ := controller.ObjectFromVariable(Kind, e.b.GetVariable("default", Kind.Path("other")))
	must.MapNotContainsKey(t, o.Labels, labelManagedBy)

	// Stopping the marked job deletes the object, and with it the proxy.
	e.b.RemoveJob("default", "api", false)
	waitFor(t, "object deleted", func() bool { return e.b.GetVariable("default", Kind.Path("api-public")) == nil })
	waitFor(t, "proxy deleted", func() bool { return e.b.GetJob("default", "lb-api-public") == nil })
	must.NotNil(t, e.b.GetVariable("default", Kind.Path("other")))
}

func TestProxy_Forwards(t *testing.T) {
	// Two echo servers that prefix their name.
	echo := func(name string) string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		must.NoError(t, err)
		t.Cleanup(func() { ln.Close() })
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() {
					defer c.Close()
					line, _ := bufio.NewReader(c).ReadString('\n')
					fmt.Fprintf(c, "%s:%s", name, line)
				}()
			}
		}()
		return ln.Addr().String()
	}
	a, b := echo("a"), echo("b")

	dir := t.TempDir()
	targets := filepath.Join(dir, "targets")
	// One unreachable target is skipped.
	must.NoError(t, os.WriteFile(targets, []byte("127.0.0.1:1\n"+a+"\n"+b+"\n"), 0o600))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must.NoError(t, err)
	p := &proxy{targetsFile: targets, logger: testLogger(), dialTimeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.serve(ctx, ln) }()

	call := func() string {
		c, err := net.Dial("tcp", ln.Addr().String())
		must.NoError(t, err)
		defer c.Close()
		fmt.Fprint(c, "hi\n")
		out, _ := io.ReadAll(c)
		return string(out)
	}
	waitFor(t, "targets loaded", func() bool { return len(p.pick()) == 3 })
	seen := map[string]bool{}
	for range 6 {
		seen[call()] = true
	}
	must.Eq(t, map[string]bool{"a:hi\n": true, "b:hi\n": true}, seen)

	// Target changes are picked up.
	must.NoError(t, os.WriteFile(targets, []byte(b+"\n"), 0o600))
	waitFor(t, "targets reloaded", func() bool { return len(p.pick()) == 1 })
	must.Eq(t, "b:hi\n", call())
}
