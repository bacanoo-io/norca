// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/uuid"
	"github.com/bacanoo-io/norca/norca/norca/admission"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/norca/structs/config"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/shoenig/test/must"
)

// Whether a resource of a release is gone is read from the state, not from
// the text of the error of its deletion (review finding JOG-6): the refusal
// to delete a quota names the namespace that uses it, a name somebody else
// chose.
func TestRelease_DeleteResourceRefused(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()
	store := c.srv.State()

	_, err := c.apply("default", "app", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "app"},
		Resources: []*structs.ReleaseResource{{Type: structs.ReleaseResourceQuota, Quota: &structs.QuotaSpec{
			Name:   "app-quota",
			Limits: []*structs.QuotaLimit{{Region: "global", RegionLimit: &structs.Resources{CPU: 1000, MemoryMB: 1000}}},
		}}},
	})
	must.NoError(t, err)
	index, err := store.LatestIndex()
	must.NoError(t, err)
	must.NoError(t, store.UpsertNamespaces(index+1, []*structs.Namespace{{Name: "nonexistent", Quota: "app-quota"}}))

	op, cancel := c.srv.releases.newOp(nil, c.token, "default", "app", 0)
	defer cancel()
	ref := &structs.ReleaseResourceRef{Type: structs.ReleaseResourceQuota, Name: "app-quota"}
	err = op.deleteResource(ref)
	must.Error(t, err, must.Sprint("a refused deletion counted as done"))
	must.StrContains(t, err.Error(), "is in use by namespace")
	quota, err := store.QuotaSpecByName(nil, "app-quota")
	must.NoError(t, err)
	must.NotNil(t, quota)

	// A token the servers don't know is refused too.
	unknown, cancelUnknown := c.srv.releases.newOp(nil, uuid.Generate(), "default", "app", 0)
	defer cancelUnknown()
	must.Error(t, unknown.deleteResource(ref))

	// A resource that is gone already is fine.
	must.NoError(t, op.deleteResource(&structs.ReleaseResourceRef{Type: structs.ReleaseResourceQuota, Name: "never-there"}))
	must.NoError(t, op.deleteResource(&structs.ReleaseResourceRef{
		Type: structs.ReleaseResourceJob, Namespace: "default", Name: "never-there"}))

	// Once the namespace no longer uses it, the quota is deleted.
	index, err = store.LatestIndex()
	must.NoError(t, err)
	must.NoError(t, store.UpsertNamespaces(index+1, []*structs.Namespace{{Name: "nonexistent"}}))
	must.NoError(t, op.deleteResource(ref))
	quota, err = store.QuotaSpecByName(nil, "app-quota")
	must.NoError(t, err)
	must.Nil(t, quota)
}

// A dry run reports the error of an object whose kind exists, even when the
// text of the error says "not found" (review finding JOG-6): only an object
// whose kind or namespace the release may still create is planned without
// the error of the endpoint.
func TestRelease_PlanObjectErrorNamingNotFound(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()

	kind := &structs.ReleaseResource{Type: structs.ReleaseResourceObjectKind, ObjectKind: &structs.ObjectKind{
		Group: "test.example.com", Kind: "Widget", Namespaced: true, Schema: testReleaseKindSchema,
	}}
	object := func(name, namespace string, labels map[string]string) *structs.ReleaseResource {
		return &structs.ReleaseResource{Type: structs.ReleaseResourceObject, Object: &structs.CustomObject{
			Group: "test.example.com", Kind: "Widget", Namespace: namespace, Name: name, Labels: labels,
			Spec: map[string]any{"size": 2},
		}}
	}
	plan := func(resources ...*structs.ReleaseResource) map[string]*structs.ReleaseResourceChange {
		t.Helper()
		resp, err := c.apply("default", "app", &structs.ReleaseApplyRequest{
			Pack: &structs.ReleasePack{Name: "app"}, Resources: resources, DryRun: true,
		})
		must.NoError(t, err)
		changes := map[string]*structs.ReleaseResourceChange{}
		for _, ch := range resp.Changes {
			changes[ch.Type+":"+ch.Name] = ch
		}
		return changes
	}

	// The kind is created by the release itself: no error yet.
	changes := plan(kind, object("w1", "", nil))
	ch := changes["object:test.example.com/Widget/w1"]
	must.NotNil(t, ch)
	must.Eq(t, structs.ReleaseChangeCreate, ch.Action)
	must.Eq(t, "", ch.Error)

	_, err := c.apply("default", "app", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "app"}, Resources: []*structs.ReleaseResource{kind},
	})
	must.NoError(t, err)

	// The kind exists; the label key is refused, and the refusal quotes it.
	changes = plan(kind, object("w2", "", map[string]string{"not found": "x"}))
	ch = changes["object:test.example.com/Widget/w2"]
	must.NotNil(t, ch)
	must.Eq(t, structs.ReleaseChangeCreate, ch.Action)
	must.StrContains(t, ch.Error, "invalid label key")

	// The namespace is created by the release itself: no error yet.
	ns := &structs.ReleaseResource{Type: structs.ReleaseResourceNamespace, Namespace: &structs.Namespace{Name: "app-ns"}}
	changes = plan(kind, ns, object("w3", "app-ns", nil))
	ch = changes["object:test.example.com/Widget/w3"]
	must.NotNil(t, ch)
	must.Eq(t, structs.ReleaseChangeCreate, ch.Action)
	must.Eq(t, "", ch.Error)
}

// A Variable of a release is written only in the state the release read
// (review finding ATM-11): a secret item the release takes from the stored
// Variable is not written back over a value somebody set meanwhile.
func TestRelease_VariableWriteIsCheckedAndSet(t *testing.T) {
	ci.Parallel(t)

	// The admission webhook of Variables runs between the read of the
	// release and its write: armed, it rotates the password there.
	var armed atomic.Bool
	var srv *Server
	var token string
	rotated := make(chan error, 1)
	hook := newTestWebhook(t, func(req *admission.ReviewRequest) *admission.ReviewResponse {
		if req.Variable != nil && req.Variable.Path == "config/app" && armed.CompareAndSwap(true, false) {
			var resp structs.VariablesApplyResponse
			rotated <- srv.RPC(structs.VariablesApplyRPCMethod, &structs.VariablesApplyRequest{
				Op: structs.VarOpSet,
				Var: &structs.VariableDecrypted{
					VariableMetadata: structs.VariableMetadata{Namespace: "default", Path: "config/app"},
					Items:            map[string]string{"user": "admin", "password": "rotated"},
				},
				WriteRequest: structs.WriteRequest{Region: "global", Namespace: "default", AuthToken: token},
			}, &resp)
		}
		return &admission.ReviewResponse{Allowed: true}
	})
	c, cleanup := newReleaseTestClientConfig(t, func(cfg *Config) {
		cfg.AdmissionConfig = &config.AdmissionConfig{Webhooks: []*config.AdmissionWebhookConfig{
			{Name: "rotate", URL: hook.URL, Resources: []string{structs.AdmissionResourceVariable}},
		}}
	})
	defer cleanup()
	srv, token = c.srv, c.token

	variable := func(user string) *structs.ReleaseResource {
		return &structs.ReleaseResource{Type: structs.ReleaseResourceVariable, SecretKeys: []string{"password"},
			Variable: &structs.VariableDecrypted{
				VariableMetadata: structs.VariableMetadata{Path: "config/app"},
				Items:            map[string]string{"user": user},
			}}
	}
	items := func() map[string]string {
		t.Helper()
		var out structs.VariablesReadResponse
		must.NoError(t, msgpackrpc.CallWithCodec(c.codec, structs.VariablesReadRPCMethod, &structs.VariablesReadRequest{
			Path:         "config/app",
			QueryOptions: structs.QueryOptions{Region: "global", Namespace: "default", AuthToken: c.token},
		}, &out))
		must.NotNil(t, out.Data)
		return out.Data.Items
	}

	_, err := c.apply("default", "app", &structs.ReleaseApplyRequest{
		Pack:      &structs.ReleasePack{Name: "app"},
		Resources: []*structs.ReleaseResource{variable("admin")},
		Secrets:   []*structs.ReleaseVariableSecret{{Path: "config/app", Items: map[string]string{"password": "s3cret"}}},
	})
	must.NoError(t, err)
	must.Eq(t, "s3cret", items()["password"])

	// The upgrade keeps the stored password; the password is rotated while
	// it writes the Variable. The upgrade fails and the rotated password
	// stays.
	armed.Store(true)
	_, err = c.apply("default", "app", &structs.ReleaseApplyRequest{
		Pack:      &structs.ReleasePack{Name: "app"},
		Resources: []*structs.ReleaseResource{variable("admin2")},
	})
	select {
	case err := <-rotated:
		must.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the webhook was not asked about the write of the upgrade")
	}
	must.ErrorContains(t, err, "changed")
	must.Eq(t, map[string]string{"user": "admin", "password": "rotated"}, items())

	// Applied again, the upgrade reads the rotated password and keeps it.
	_, err = c.apply("default", "app", &structs.ReleaseApplyRequest{
		Pack:      &structs.ReleasePack{Name: "app"},
		Resources: []*structs.ReleaseResource{variable("admin2")},
	})
	must.NoError(t, err)
	must.Eq(t, map[string]string{"user": "admin2", "password": "rotated"}, items())
}
