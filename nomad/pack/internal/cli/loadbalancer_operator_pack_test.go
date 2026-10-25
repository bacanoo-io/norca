// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package cli

import (
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/command/agent"
	"github.com/shoenig/test/must"

	ct "github.com/hashicorp/nomad/pack/internal/cli/testhelper"
)

// The tests in this file install the LoadBalancer operator pack of the
// repository (packs/loadbalancer-operator) as a release on a test agent
// with the mock driver: the kind with its limits and webhooks, the ACL
// policy bound to the controller job, the admission policy and the
// controller job. The controller itself runs in the end-to-end test of the
// controller module (examples/loadbalancer).

var lbPackVars = []string{"--var=driver=mock_driver", "--var=proxy_driver=mock_driver", "--name=lb", "--detach"}

// lbWebhook is the webhook server of the test: it fills in the class "edge"
// and rejects port 22.
func lbWebhook(t *testing.T) *httptest.Server {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var review struct {
			Request struct {
				UID    string `json:"uid"`
				Object struct {
					Spec map[string]any `json:"spec"`
				} `json:"object"`
			} `json:"request"`
		}
		must.NoError(t, json.Unmarshal(body, &review))
		resp := map[string]any{"uid": review.Request.UID, "allowed": true}
		spec := review.Request.Object.Spec
		switch r.URL.Path {
		case "/default":
			if _, ok := spec["class"]; !ok {
				patch, _ := json.Marshal([]map[string]any{{"op": "add", "path": "/spec/class", "value": "edge"}})
				resp["patch"] = json.RawMessage(patch)
			}
		case "/validate":
			if spec["port"] == float64(22) {
				resp["allowed"] = false
				resp["message"] = "port 22 is reserved"
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"api_version": "admission.norca.io/v1", "kind": "AdmissionReview", "response": resp})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCLI_LoadBalancerOperatorPack(t *testing.T) {
	isolateSupplyChain(t)
	packDir := repoPack(t, "loadbalancer-operator")

	// The variables schema rejects invalid values before rendering.
	r := runPackCmd(t, []string{"render", packDir, "--var=driver=podman"})
	must.NonZero(t, r.exitCode)
	must.StrContains(t, r.cmdOut.String()+r.cmdErr.String(), "/driver")

	r = runPackCmd(t, []string{"render", packDir})
	expectExit(t, r, 0)
	out := r.cmdOut.String()
	must.StrContains(t, out, "loadbalancer.kind.hcl")
	must.StrContains(t, out, "controller.acl.hcl")
	must.StrContains(t, out, "service-marks.admission.hcl")
	must.StrContains(t, out, `"default": "nomad"`)
	must.StrNotContains(t, out, `webhook "validate"`)

	ws := lbWebhook(t)
	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ws.Certificate().Raw}))
	varFile := filepath.Join(t.TempDir(), "webhook.hcl")
	must.NoError(t, os.WriteFile(varFile, []byte("webhook_enabled = true\nwebhook_url = \""+ws.URL+"\"\nwebhook_ca_cert = <<EOT\n"+caPEM+"EOT\n"), 0o600))

	ct.HTTPTestWithACL(t, ct.WithDefaultConfig(), func(s *agent.TestAgent) {
		token := "--token=" + s.Config.Client.Meta["token"]
		client, err := ct.NewTestClient(s)
		must.NoError(t, err)

		r := runTestPackCmd(t, s, append([]string{"run", packDir, token}, lbPackVars...))
		expectExit(t, r, 0)
		must.StrContains(t, r.cmdOut.String(), `Release "lb" deployed at revision 1`)

		// The kind, with its limits.
		kind, _, err := client.ObjectKinds().Info("norca.io", "LoadBalancer", nil)
		must.NoError(t, err)
		must.Eq(t, uint64(1), kind.Version)
		must.SliceEmpty(t, kind.Webhooks)
		must.NotNil(t, kind.Limits)
		must.Eq(t, 50.0, kind.Limits.WriteRate)
		must.Eq(t, 20.0, kind.Limits.IdentityWriteRate)
		must.Eq(t, 16, kind.Limits.MaxWatchesPerIdentity)

		// The ACL policy is bound to the controller job.
		policy, _, err := client.ACLPolicies().Info("loadbalancer-controller", nil)
		must.NoError(t, err)
		must.NotNil(t, policy.JobACL)
		must.Eq(t, "loadbalancer-controller", policy.JobACL.JobID)
		must.StrContains(t, policy.Rules, `object "norca.io/LoadBalancer"`)

		job, _, err := client.Jobs().Info("loadbalancer-controller", nil)
		must.NoError(t, err)
		must.Eq(t, "mock_driver", job.TaskGroups[0].Tasks[0].Driver)
		must.Eq(t, "lb", job.Meta[api.ReleaseMetaRelease])

		// Objects get the schema's defaults.
		obj, _, err := client.Objects().Apply(&api.CustomObject{
			Group: "norca.io", Kind: "LoadBalancer", Namespace: "default", Name: "web",
			Spec: map[string]any{"service": "web", "port": 8080},
		}, nil, nil)
		must.NoError(t, err)
		must.Eq[any](t, "nomad", obj.Spec["class"])
		must.Eq[any](t, float64(1), obj.Spec["replicas"])

		// The admission policy checks the marks of services.
		bad := markedJob("marked", "99999")
		_, _, err = client.Jobs().Register(bad, nil)
		must.ErrorContains(t, err, "norca_lb_port")
		_, _, err = client.Jobs().Register(markedJob("marked", "443"), nil)
		must.NoError(t, err)
		_, _, err = client.Jobs().Deregister("marked", true, nil)
		must.NoError(t, err)

		// Upgrade with the webhooks: applied with the management token, so
		// they are approved at once.
		r = runTestPackCmd(t, s, append([]string{"upgrade", packDir, token, "--var-file=" + varFile}, lbPackVars...))
		expectExit(t, r, 0)
		kind, _, err = client.ObjectKinds().Info("norca.io", "LoadBalancer", nil)
		must.NoError(t, err)
		must.Len(t, 2, kind.Webhooks)
		for _, w := range kind.Webhooks {
			must.True(t, w.Approved(), must.Sprintf("webhook %s", w.Name))
		}
		job, _, err = client.Jobs().Info("loadbalancer-controller", nil)
		must.NoError(t, err)
		must.Len(t, 2, job.TaskGroups[0].Tasks)

		// The defaulting webhook fills in the class, the validating one
		// rejects port 22.
		obj, _, err = client.Objects().Apply(&api.CustomObject{
			Group: "norca.io", Kind: "LoadBalancer", Namespace: "default", Name: "edge",
			Spec: map[string]any{"service": "web", "port": 9090},
		}, nil, nil)
		must.NoError(t, err)
		must.Eq[any](t, "edge", obj.Spec["class"])
		_, _, err = client.Objects().Apply(&api.CustomObject{
			Group: "norca.io", Kind: "LoadBalancer", Namespace: "default", Name: "ssh",
			Spec: map[string]any{"service": "web", "port": 22},
		}, nil, nil)
		must.ErrorContains(t, err, "port 22 is reserved")

		// The approvals are server state: the release does not drift.
		r = runTestPackCmd(t, s, []string{"diff", "lb", "--live", token})
		expectExit(t, r, 0)

		// Uninstall: the kind can only go once its objects are gone.
		for _, name := range []string{"web", "edge"} {
			_, _, err := client.Objects().Delete("norca.io", "LoadBalancer", name, nil, nil)
			must.NoError(t, err)
		}
		r = runTestPackCmd(t, s, []string{"uninstall", "lb", token})
		expectExit(t, r, 0)
		_, _, err = client.ObjectKinds().Info("norca.io", "LoadBalancer", nil)
		must.ErrorContains(t, err, "404")
		_, _, err = client.ACLPolicies().Info("loadbalancer-controller", nil)
		must.Error(t, err)
		policies, _, err := client.AdmissionPolicies().List(nil)
		must.NoError(t, err)
		must.SliceEmpty(t, policies)
	})
}

// markedJob returns a mock driver job with a service marked for the
// LoadBalancer operator.
func markedJob(id, port string) *api.Job {
	job := api.NewServiceJob(id, id, "global", 50)
	job.Datacenters = []string{"*"}
	task := api.NewTask("app", "mock_driver")
	task.Config = map[string]any{"run_for": "1h"}
	group := api.NewTaskGroup("app", 1)
	group.Services = []*api.Service{{
		Name:     id,
		Provider: "nomad",
		Meta:     map[string]string{"norca_lb_class": "nomad", "norca_lb_port": port},
	}}
	group.AddTask(task)
	job.AddTaskGroup(group)
	return job
}
