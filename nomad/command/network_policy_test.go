// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/mitchellh/cli"
	"github.com/shoenig/test/must"
)

func TestNetworkPolicyCommand_Implements(t *testing.T) {
	ci.Parallel(t)
	var _ cli.Command = &NetworkPolicyCommand{}
	var _ cli.Command = &NetworkPolicyApplyCommand{}
	var _ cli.Command = &NetworkPolicyListCommand{}
	var _ cli.Command = &NetworkPolicyStatusCommand{}
	var _ cli.Command = &NetworkPolicyDeleteCommand{}
}

func TestParseNetworkPolicy_SpecExample(t *testing.T) {
	ci.Parallel(t)

	src := `
name      = "db-only-from-api"
namespace = "prod"

selector { app = "postgres" }

ingress {
  from {
    selector { app = "api" }
  }
  ports = ["5432/tcp"]
}

egress {
  to {
    cidr = ["10.20.0.0/16"]
  }
  ports = ["443/tcp"]
}

enforcement = "required"   # or "best_effort"
`
	policy, err := parseNetworkPolicy("policy.hcl", []byte(src), false)
	must.NoError(t, err)
	must.Eq(t, &api.NetworkPolicy{
		Name:      "db-only-from-api",
		Namespace: "prod",
		Selector:  map[string]string{"app": "postgres"},
		Ingress: []*api.NetworkPolicyRule{{
			Peers: []*api.NetworkPolicyPeer{{Selector: map[string]string{"app": "api"}}},
			Ports: []string{"5432/tcp"},
		}},
		Egress: []*api.NetworkPolicyRule{{
			Peers: []*api.NetworkPolicyPeer{{CIDR: []string{"10.20.0.0/16"}}},
			Ports: []string{"443/tcp"},
		}},
		Enforcement: "required",
	}, policy)
}

func TestParseNetworkPolicy_Errors(t *testing.T) {
	ci.Parallel(t)

	cases := map[string]string{
		"to in ingress":     `name = "x"` + "\n" + `ingress { to { cidr = ["10.0.0.0/8"] } }`,
		"from in egress":    `name = "x"` + "\n" + `egress { from { cidr = ["10.0.0.0/8"] } }`,
		"admin field":       `name = "x"` + "\n" + `priority = 1`,
		"unknown attribute": `name = "x"` + "\n" + `bogus = 1`,
		"selector object":   `name = "x"` + "\n" + `selector { app = { a = 1 } }`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseNetworkPolicy("p.hcl", []byte(src), false)
			must.Error(t, err)
		})
	}
}

func TestParseAdminNetworkPolicy(t *testing.T) {
	ci.Parallel(t)

	src := `
name       = "no-metadata-service"
priority   = 10
namespaces = ["*"]

egress {
  name   = "metadata"
  action = "deny"
  to {
    cidr = ["169.254.169.254/32"]
  }
}

ingress {
  action = "pass"
  from {
    namespace = "*"
    selector { tier = "web" }
  }
}
`
	policy, err := parseAdminNetworkPolicy("admin.hcl", []byte(src), false)
	must.NoError(t, err)
	must.Eq(t, 10, policy.Priority)
	must.Eq(t, []string{"*"}, policy.Namespaces)
	must.Eq(t, "deny", policy.Egress[0].Action)
	must.Eq(t, "metadata", policy.Egress[0].Name)
	must.Eq(t, "*", policy.Ingress[0].Peers[0].Namespace)
	must.Eq(t, map[string]string{"tier": "web"}, policy.Ingress[0].Peers[0].Selector)

	_, err = parseAdminNetworkPolicy("admin.hcl", []byte(`name = "x"`+"\n"+`namespace = "prod"`), false)
	must.ErrorContains(t, err, "not valid for admin network policies")

	// JSON input in the API form.
	policy, err = parseAdminNetworkPolicy("admin.json", []byte(`{"Name":"j","Priority":3}`), true)
	must.NoError(t, err)
	must.Eq(t, 3, policy.Priority)
}

func TestFormatNetworkPolicyRules(t *testing.T) {
	ci.Parallel(t)

	rows := formatNetworkPolicyRules("egress", []*api.NetworkPolicyRule{
		{Peers: []*api.NetworkPolicyPeer{{CIDR: []string{"10.0.0.0/8"}}}, Ports: []string{"443/tcp"}},
		{Name: "all"},
	})
	must.Eq(t, []string{
		"egress|#0|allow|10.0.0.0/8|443/tcp",
		"egress|all|allow|<any>|<any>",
	}, rows)
	must.Eq(t, "a=1,b=2", formatNetworkPolicySelector(map[string]string{"b": "2", "a": "1"}))
}
