// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/nomad/structs"
)

// jobSecretsHook handles the secret delivery blocks of jobs (Norca,
// norca-doc content/docs/design/secrets-plan.md, "Provider resolution and fallback" and
// "Permissions"):
//
//   - as a mutator it captures the namespace's default secret provider
//     (the namespace metadata "secrets.default_provider") on the job, like
//     the namespace's default node pool, so the clients resolve step 3 of the
//     lookups without reading the namespace, and admission policies see it.
//     A changed namespace default applies to jobs registered afterwards.
//   - as a validator it gates the blocks on the servers' version.
//
// The secret_access bindings are authorized against the submitter's ACL by
// checkSecretAccessGrants in Job.Register and Job.Plan.
type jobSecretsHook struct {
	srv *Server
}

func (jobSecretsHook) Name() string { return "secrets" }

// Mutate sets the namespace default provider on the job; a submitted value
// is replaced.
func (h jobSecretsHook) Mutate(job *structs.Job) (*structs.Job, []error, error) {
	if h.srv == nil || h.srv.fsm == nil {
		// A server without state (unit tests of other hooks).
		return job, nil, nil
	}
	var def string
	ns, err := h.srv.State().NamespaceByName(nil, job.Namespace)
	if err != nil {
		return nil, nil, err
	}
	if ns != nil {
		def = strings.TrimSpace(ns.Meta[structs.NamespaceMetaSecretsDefaultProvider])
	}
	switch {
	case def != "":
		if job.Secrets == nil {
			job.Secrets = &structs.SecretsConfig{}
		}
		job.Secrets.NamespaceDefaultProvider = def
	case job.Secrets != nil:
		job.Secrets.NamespaceDefaultProvider = ""
		if job.Secrets.DefaultProvider == "" {
			job.Secrets = nil
		}
	}
	return job, nil, nil
}

// Validate requires servers that understand the secret delivery blocks:
// older servers would drop the secret_access bindings and defaults.
func (h jobSecretsHook) Validate(job *structs.Job) ([]error, error) {
	if !job.UsesSecretDelivery() || h.srv == nil || h.srv.serf == nil {
		return nil, nil
	}
	if !ServersMeetMinimumNorcaVersion(h.srv.Members(), h.srv.Region(), false) {
		return nil, fmt.Errorf("all servers in the region must run a Norca version that supports secret delivery")
	}
	if p := job.Secrets; p != nil && p.NamespaceDefaultProvider != "" &&
		!validSecretsProviderName(p.NamespaceDefaultProvider) {
		return nil, fmt.Errorf("namespace %q: invalid %s %q", job.Namespace,
			structs.NamespaceMetaSecretsDefaultProvider, p.NamespaceDefaultProvider)
	}
	return nil, nil
}

func validSecretsProviderName(name string) bool {
	return (&structs.SecretsConfig{DefaultProvider: name}).Validate() == nil
}

// checkSecretAccessGrants authorizes the secret_access bindings of a job:
// the submitter must hold read-secret on each binding's path pattern in the
// job's namespace (write-secret is not enough: the binding hands the job
// read access to the current values). A job can therefore never grant its
// workload identities access its submitter doesn't have.
func checkSecretAccessGrants(aclObj *acl.ACL, job *structs.Job) error {
	if aclObj == nil {
		return nil
	}
	for _, g := range job.SecretAccessGrants() {
		if aclObj.AllowSecretGrant(job.Namespace, g.Pattern, acl.NamespaceCapabilityReadSecret) {
			continue
		}
		where := fmt.Sprintf("group %q", g.Group)
		if g.Task != "" {
			where = fmt.Sprintf("task %q of group %q", g.Task, g.Group)
		}
		return structs.NewErrRPCCodedf(http.StatusForbidden,
			"%s: secret_access %q of %s requires read-secret on the path for the submitter",
			structs.ErrPermissionDenied, g.Pattern, where)
	}
	return nil
}

// secretAccessGranted reports whether the secret_access bindings of the
// allocation's job grant the workload identity the capability on the path:
// read-secret and the list-secrets it implies, in the job's namespace.
func secretAccessGranted(alloc *structs.Allocation, claims *structs.IdentityClaims, namespace, path, capability string) bool {
	if alloc == nil || alloc.Job == nil || claims == nil {
		return false
	}
	if capability != acl.NamespaceCapabilityReadSecret && capability != acl.NamespaceCapabilityListSecrets {
		return false
	}
	if namespace != alloc.Namespace {
		return false
	}
	for _, pattern := range structs.SecretAccessPatterns(alloc.Job, alloc.TaskGroup, claims.TaskName) {
		if secretPathMatch(pattern, path) {
			return true
		}
	}
	return false
}

// secretPathMatch matches a secret path against a pattern in which "*"
// matches any sequence of characters, as in ACL path rules.
func secretPathMatch(pattern, path string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == path
	}
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(path, parts[0]) {
		return false
	}
	rest := path[len(parts[0]):]
	for i, part := range parts[1:] {
		if i == len(parts)-2 {
			return strings.HasSuffix(rest, part) && len(rest) >= len(part)
		}
		idx := strings.Index(rest, part)
		if idx < 0 {
			return false
		}
		rest = rest[idx+len(part):]
	}
	return true
}
