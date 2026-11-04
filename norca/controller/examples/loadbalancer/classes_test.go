// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/controller"
	"github.com/shoenig/test/must"
)

// fakeProvider is an external load balancer provider.
type fakeProvider struct {
	srv *httptest.Server

	mu       sync.Mutex
	puts     []remoteRequest
	deletes  []string
	auth     []string
	ready    bool
	failPuts bool
}

func newFakeProvider(t *testing.T) *fakeProvider {
	p := &fakeProvider{ready: true}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.auth = append(p.auth, r.Header.Get("Authorization"))
		switch r.Method {
		case http.MethodPut:
			if p.failPuts {
				http.Error(w, "cloud API down", http.StatusBadGateway)
				return
			}
			body, _ := io.ReadAll(r.Body)
			var req remoteRequest
			if err := json.Unmarshal(body, &req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			p.puts = append(p.puts, req)
			_ = json.NewEncoder(w).Encode(remoteResponse{
				Ready: p.ready, Addresses: []string{"203.0.113.10:443"}, Resource: "lb-" + req.Name,
			})
		case http.MethodDelete:
			p.deletes = append(p.deletes, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakeProvider) putCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.puts)
}

func (p *fakeProvider) lastPut() remoteRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.puts) == 0 {
		return remoteRequest{}
	}
	return p.puts[len(p.puts)-1]
}

func (p *fakeProvider) deleted() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.deletes...)
}

func (p *fakeProvider) setFail(fail bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failPuts = fail
}

func (e *env) putClass(name string, spec map[string]any) {
	e.b.PutObject(&api.CustomObject{Group: ClassKind.Group, Kind: ClassKind.Kind, Name: name, Spec: spec})
}

func (e *env) classHeartbeat(t *testing.T, name string, at time.Time) {
	_, err := e.b.UpdateObjectStatus(context.Background(), ClassKind, "", name,
		map[string]any{"lastHeartbeat": at.UTC().Format(time.RFC3339)}, 0)
	must.NoError(t, err)
}

func (e *env) readyCondition(t *testing.T, name string) (reason, message string) {
	c := e.nativeStatus(t, name).Condition(controller.ConditionReady)
	if c == nil {
		return "", ""
	}
	return c.Reason, c.Message
}

func (e *env) waitReason(t *testing.T, name, reason string) string {
	t.Helper()
	waitFor(t, name+" "+reason, func() bool {
		got, _ := e.readyCondition(t, name)
		return got == reason
	})
	_, msg := e.readyCondition(t, name)
	return msg
}

func TestLoadBalancerClass_RemoteProvider(t *testing.T) {
	e := startOperatorWith(t, false, controller.StorageNative)
	prov := newFakeProvider(t)
	e.b.PutVariable(&api.Variable{Path: "lb/hcloud/token", Namespace: "default", Items: api.VariableItems{"token": "s3cret"}})
	e.putClass("hcloud-lb", map[string]any{
		"endpoint":      prov.srv.URL,
		"tokenVariable": "lb/hcloud/token",
		"capabilities":  map[string]any{"internal": true, "ports": "80,443"},
		"parameters":    map[string]any{"location": "fsn1"},
	})
	e.cluster.setTargets("default", "web", Target{Address: "10.0.0.1", Port: 8001, AllocID: "a1"})
	e.putNativeLB("web", map[string]any{"service": "web", "port": 443, "class": "hcloud-lb"})

	waitFor(t, "ready", func() bool { return e.nativeStatus(t, "web").IsTrue(controller.ConditionReady) })
	st := e.nativeStatus(t, "web")
	must.Eq(t, "hcloud-lb", st.Class)
	must.Eq(t, []string{"203.0.113.10:443"}, st.Addresses)
	must.Eq(t, "lb-web", st.Resource)

	// The provider got the object, the targets and its parameters, with the
	// token of the class; no proxy job was created for it.
	req := prov.lastPut()
	must.Eq(t, remoteRequest{
		Name: "web", Namespace: "default", Service: "web", Port: 443,
		Targets:    []remoteTarget{{Address: "10.0.0.1", Port: 8001, AllocID: "a1"}},
		Parameters: map[string]any{"location": "fsn1"},
	}, req)
	must.Eq(t, "Bearer s3cret", prov.auth[0])
	must.Nil(t, e.b.GetJob("default", "lb-web"))

	// A target change is sent to the provider.
	e.cluster.setTargets("default", "web", Target{Address: "10.0.0.1", Port: 8001}, Target{Address: "10.0.0.2", Port: 8001})
	e.b.Publish(api.Event{Topic: api.TopicService, Type: "ServiceRegistration", Key: "svc-2", Namespace: "default",
		Payload: map[string]any{"Service": map[string]any{"ServiceName": "web", "Namespace": "default"}}})
	waitFor(t, "second target", func() bool { return len(prov.lastPut().Targets) == 2 })

	// Deleting the object calls the provider before the finalizer goes.
	e.b.RemoveObject(Kind, "default", "web")
	waitFor(t, "object deleted", func() bool { return e.nativeObject(t, "web") == nil })
	must.Eq(t, []string{"/v1/loadbalancers/default/web"}, prov.deleted())
}

func TestLoadBalancerClass_UnknownClassAppears(t *testing.T) {
	e := startOperatorWith(t, false, controller.StorageNative)
	e.cluster.setTargets("default", "web", Target{Address: "10.0.0.1", Port: 8001})
	e.putNativeLB("web", map[string]any{"service": "web", "port": 443, "class": "hcloud-lb"})

	msg := e.waitReason(t, "web", "UnknownClass")
	must.StrContains(t, msg, `no provider for class "hcloud-lb"`)
	must.StrContains(t, msg, "known: nomad")

	// The provider starts and registers: the load balancer is reconciled
	// without any change of its own.
	prov := newFakeProvider(t)
	e.putClass("hcloud-lb", map[string]any{"endpoint": prov.srv.URL})
	waitFor(t, "ready", func() bool { return e.nativeStatus(t, "web").IsTrue(controller.ConditionReady) })
	must.Positive(t, prov.putCount())
}

func TestLoadBalancerClass_Heartbeat(t *testing.T) {
	e := startOperatorWith(t, false, controller.StorageNative)
	prov := newFakeProvider(t)
	e.putClass("hcloud-lb", map[string]any{"endpoint": prov.srv.URL})
	e.classHeartbeat(t, "hcloud-lb", time.Now().Add(-10*time.Minute))
	e.cluster.setTargets("default", "web", Target{Address: "10.0.0.1", Port: 8001})
	e.putNativeLB("web", map[string]any{"service": "web", "port": 443, "class": "hcloud-lb"})

	// A provider that stopped reporting is not called.
	msg := e.waitReason(t, "web", "ProviderUnavailable")
	must.StrContains(t, msg, "no heartbeat")
	must.Eq(t, 0, prov.putCount())

	// It comes back.
	e.classHeartbeat(t, "hcloud-lb", time.Now())
	waitFor(t, "ready", func() bool { return e.nativeStatus(t, "web").IsTrue(controller.ConditionReady) })
}

func TestLoadBalancerClass_Capabilities(t *testing.T) {
	e := startOperatorWith(t, false, controller.StorageNative)
	prov := newFakeProvider(t)
	e.putClass("hcloud-lb", map[string]any{
		"endpoint":     prov.srv.URL,
		"capabilities": map[string]any{"ports": "80,443,8000-8100"},
	})
	e.cluster.setTargets("default", "web", Target{Address: "10.0.0.1", Port: 8001})

	e.putNativeLB("internal", map[string]any{"service": "web", "port": 443, "class": "hcloud-lb", "internal": true})
	must.StrContains(t, e.waitReason(t, "internal", "Unsupported"), "cannot create internal")

	e.putNativeLB("port", map[string]any{"service": "web", "port": 9000, "class": "hcloud-lb"})
	must.StrContains(t, e.waitReason(t, "port", "Unsupported"), "does not support port 9000")

	e.putNativeLB("range", map[string]any{"service": "web", "port": 8050, "class": "hcloud-lb"})
	waitFor(t, "range", func() bool { return e.nativeStatus(t, "range").IsTrue(controller.ConditionReady) })
	must.Eq(t, "range", prov.lastPut().Name)
}

func TestLoadBalancerClass_ProviderFailure(t *testing.T) {
	e := startOperatorWith(t, false, controller.StorageNative)
	prov := newFakeProvider(t)
	prov.setFail(true)
	e.putClass("hcloud-lb", map[string]any{"endpoint": prov.srv.URL})
	e.cluster.setTargets("default", "web", Target{Address: "10.0.0.1", Port: 8001})
	e.putNativeLB("web", map[string]any{"service": "web", "port": 443, "class": "hcloud-lb"})

	// The failure is retried with backoff and shows in the event log.
	waitFor(t, "event", func() bool {
		for _, ev := range e.b.ObjectEvents(Kind, "default", "web") {
			if ev.Reason == "ProvisioningFailed" && strings.Contains(ev.Message, "answered 502") {
				return true
			}
		}
		return false
	})
	must.False(t, e.nativeStatus(t, "web").IsTrue(controller.ConditionReady))

	prov.setFail(false)
	waitFor(t, "ready", func() bool { return e.nativeStatus(t, "web").IsTrue(controller.ConditionReady) })
}

func TestLoadBalancerClass_RemovedWhileObjectExists(t *testing.T) {
	e := startOperatorWith(t, false, controller.StorageNative)
	prov := newFakeProvider(t)
	e.putClass("hcloud-lb", map[string]any{"endpoint": prov.srv.URL})
	e.cluster.setTargets("default", "web", Target{Address: "10.0.0.1", Port: 8001})
	e.putNativeLB("web", map[string]any{"service": "web", "port": 443, "class": "hcloud-lb"})
	waitFor(t, "ready", func() bool { return e.nativeStatus(t, "web").IsTrue(controller.ConditionReady) })

	// The class disappears: deleting the load balancer keeps its finalizer,
	// because the provider cannot be told to remove it.
	e.b.RemoveObject(ClassKind, "", "hcloud-lb")
	time.Sleep(50 * time.Millisecond) // the registry caches the list for 10ms here
	e.b.RemoveObject(Kind, "default", "web")
	time.Sleep(300 * time.Millisecond)
	obj := e.nativeObject(t, "web")
	must.NotNil(t, obj)
	must.True(t, obj.HasFinalizer(Finalizer))

	// The provider registers again and the cleanup completes.
	e.putClass("hcloud-lb", map[string]any{"endpoint": prov.srv.URL})
	waitFor(t, "object deleted", func() bool { return e.nativeObject(t, "web") == nil })
	must.Eq(t, []string{"/v1/loadbalancers/default/web"}, prov.deleted())
}

func TestClassInfo_Validate(t *testing.T) {
	cases := map[string]struct {
		spec ClassSpec
		ok   bool
	}{
		"https":      {ClassSpec{Endpoint: "https://lb.example.com:8443"}, true},
		"http":       {ClassSpec{Endpoint: "http://lb.example.com"}, false},
		"no host":    {ClassSpec{Endpoint: "https://"}, false},
		"empty":      {ClassSpec{}, false},
		"bad ports":  {ClassSpec{Endpoint: "https://lb.example.com", Capabilities: ClassCapabilities{Ports: "443,70000"}}, false},
		"port range": {ClassSpec{Endpoint: "https://lb.example.com", Capabilities: ClassCapabilities{Ports: "80,8000-8100"}}, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := (&classInfo{Name: "x", Spec: tc.spec}).validate(false)
			if tc.ok {
				must.NoError(t, err)
			} else {
				must.Error(t, err)
			}
		})
	}
	// http is allowed for development
	must.NoError(t, (&classInfo{Name: "x", Spec: ClassSpec{Endpoint: "http://127.0.0.1:1"}}).validate(true))
}

// TestLoadBalancerClass_KindFiles checks that the kind definition and the
// schema of the registry match the types.
func TestLoadBalancerClass_KindFiles(t *testing.T) {
	raw, err := os.ReadFile("loadbalancerclass.schema.json")
	must.NoError(t, err)
	var schema struct {
		Properties map[string]struct {
			Properties map[string]map[string]any `json:"properties"`
		} `json:"properties"`
	}
	must.NoError(t, json.Unmarshal(raw, &schema))
	for _, field := range []string{"endpoint", "tokenVariable", "capabilities", "parameters"} {
		must.MapContainsKey(t, schema.Properties["spec"].Properties, field)
	}
	for _, field := range []string{"lastHeartbeat", "version"} {
		must.MapContainsKey(t, schema.Properties["status"].Properties, field)
	}

	kind, err := os.ReadFile("loadbalancerclass.kind.hcl")
	must.NoError(t, err)
	must.StrContains(t, string(kind), `group       = "`+ClassKind.Group+`"`)
	must.StrContains(t, string(kind), `kind "`+ClassKind.Kind+`"`)
	must.StrContains(t, string(kind), "namespaced  = false")
}
