// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/shoenig/test/must"
)

func TestKind_Paths(t *testing.T) {
	k := Kind{Group: "db.example.com", Kind: "PostgresCluster"}
	must.NoError(t, k.Validate())
	must.Eq(t, "db-example-com", k.PathGroup())
	must.Eq(t, "crd/db-example-com/PostgresCluster/", k.Prefix())
	must.Eq(t, "crd/db-example-com/PostgresCluster/billing", k.Path("billing"))
	must.Eq(t, "crd/db-example-com/PostgresCluster/*", k.WatchPattern())

	name, ok := k.NameFromPath("crd/db-example-com/PostgresCluster/billing")
	must.True(t, ok)
	must.Eq(t, "billing", name)

	for _, p := range []string{
		"crd/db-example-com/PostgresCluster/",
		"crd/db-example-com/PostgresCluster/a/b",
		"crd/db-example-com/Other/billing",
		"nomad/jobs/x",
	} {
		_, ok := k.NameFromPath(p)
		must.False(t, ok, must.Sprint(p))
	}

	must.Error(t, Kind{Group: "Upper.case", Kind: "X"}.Validate())
	must.Error(t, Kind{Group: "a.b", Kind: "1x"}.Validate())
	must.Error(t, Kind{Group: "a.b", Kind: "x.y"}.Validate())
	must.Error(t, ValidateName("a.b"))
	must.Error(t, ValidateName("a/b"))
	must.NoError(t, ValidateName("web-1_x~"))
}

func TestObject_VariableRoundTrip(t *testing.T) {
	k := Kind{Group: "norca.io", Kind: "LoadBalancer"}
	v := &api.Variable{
		Namespace:   "prod",
		Path:        "crd/norca-io/LoadBalancer/web",
		CreateIndex: 5,
		ModifyIndex: 9,
		Items: api.VariableItems{
			"spec":   `{"service": "web", "port": 80}`,
			"status": `{"observedSpecHash":"x"}`,
			"labels": `{"team":"a"}`,
			"note":   "kept",
		},
	}
	obj, err := ObjectFromVariable(k, v)
	must.NoError(t, err)
	must.Eq(t, ObjectKey{Namespace: "prod", Name: "web"}, obj.Key)
	must.Eq(t, map[string]string{"team": "a"}, obj.Labels)
	must.Eq(t, map[string]string{"note": "kept"}, obj.Extra)

	var spec struct {
		Service string `json:"service"`
		Port    int    `json:"port"`
	}
	must.NoError(t, obj.DecodeSpec(&spec))
	must.Eq(t, "web", spec.Service)
	must.Eq(t, 80, spec.Port)

	back, err := obj.ToVariable()
	must.NoError(t, err)
	must.Eq(t, v.Path, back.Path)
	must.Eq(t, v.Namespace, back.Namespace)
	must.Eq(t, uint64(9), back.ModifyIndex)
	must.Eq(t, v.Items, back.Items)

	// Invalid spec JSON is rejected, an invalid status is dropped.
	v.Items["spec"] = "{"
	_, err = ObjectFromVariable(k, v)
	must.Error(t, err)
	v.Items["spec"] = "{}"
	v.Items["status"] = "{"
	obj, err = ObjectFromVariable(k, v)
	must.NoError(t, err)
	must.Len(t, 0, obj.Status)
}

func TestObject_SpecHash(t *testing.T) {
	a := &Object{Spec: json.RawMessage(`{"a":1,"b":[1,2]}`)}
	b := &Object{Spec: json.RawMessage("{ \"b\": [1, 2],\n \"a\": 1 }")}
	c := &Object{Spec: json.RawMessage(`{"a":2,"b":[1,2]}`)}
	must.Eq(t, a.SpecHash(), b.SpecHash())
	must.NotEq(t, a.SpecHash(), c.SpecHash())
	must.True(t, specEqual(a, b))
	must.False(t, specEqual(a, c))

	// Status-only changes are not spec changes.
	d := b.Copy()
	d.Status = json.RawMessage(`{"x":1}`)
	must.False(t, SpecChanged(b, d))
	d.Status = nil
	must.True(t, SpecChanged(b, d))
}

func TestCommonStatus_SetCondition(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var s CommonStatus
	s.SetCondition(Condition{Type: ConditionReady, Status: ConditionFalse, Reason: "Pending"}, t0)
	must.Eq(t, t0, s.Condition(ConditionReady).LastTransitionTime)

	// Same status: the transition time is kept.
	s.SetCondition(Condition{Type: ConditionReady, Status: ConditionFalse, Reason: "Still"}, t0.Add(time.Minute))
	must.Eq(t, t0, s.Condition(ConditionReady).LastTransitionTime)
	must.Eq(t, "Still", s.Condition(ConditionReady).Reason)

	// New status: new transition time.
	s.SetCondition(Condition{Type: ConditionReady, Status: ConditionTrue}, t0.Add(2*time.Minute))
	must.Eq(t, t0.Add(2*time.Minute), s.Condition(ConditionReady).LastTransitionTime)
	must.True(t, s.IsTrue(ConditionReady))
	must.False(t, s.IsTrue(ConditionDegraded))
	must.Len(t, 1, s.Conditions)
}

func TestOwnerRef(t *testing.T) {
	k := Kind{Group: "norca.io", Kind: "LoadBalancer"}
	obj := &Object{Kind: k, Key: ObjectKey{Namespace: "prod", Name: "web"}}
	job := &api.Job{}
	SetOwner(job, obj)
	must.Eq(t, "prod/crd/norca-io/LoadBalancer/web", job.Meta[OwnerMetaKey])
	must.True(t, IsOwnedBy(job.Meta, obj))

	key, ok := OwnerFromMeta(k, job.Meta)
	must.True(t, ok)
	must.Eq(t, obj.Key, key)

	_, ok = OwnerFromMeta(Kind{Group: "norca.io", Kind: "Other"}, job.Meta)
	must.False(t, ok)
	_, ok = OwnerFromMeta(k, map[string]string{OwnerMetaKey: "crd/norca-io/LoadBalancer/web"})
	must.False(t, ok)
}

func TestConfigureUnixSocket(t *testing.T) {
	conf := api.DefaultConfig()
	must.NoError(t, configureUnixSocket(conf, "http://127.0.0.1:4646"))
	must.Nil(t, conf.HttpClient)

	must.NoError(t, configureUnixSocket(conf, "unix:///secrets/api.sock"))
	must.Eq(t, "http://127.0.0.1", conf.Address)
	must.NotNil(t, conf.HttpClient)
	must.NoError(t, configureUnixSocket(conf, `unix://C:\alloc\task\secrets/api.sock`))
	must.Error(t, configureUnixSocket(conf, "unix://"))

	// A real request over a unix socket.
	dir := t.TempDir()
	sock := filepath.Join(dir, "api.sock")
	ln, err := net.Listen("unix", sock)
	must.NoError(t, err)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`"127.0.0.1:4647"`))
	}), ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()
	conf = api.DefaultConfig()
	must.NoError(t, configureUnixSocket(conf, "unix://"+sock))
	client, err := api.NewClient(conf)
	must.NoError(t, err)
	leader, err := client.Status().Leader()
	must.NoError(t, err)
	must.Eq(t, "127.0.0.1:4647", leader)
}
