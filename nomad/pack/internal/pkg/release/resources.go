// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package release turns the rendered templates of a pack into the
// resources of a Norca release (extended package format) and holds the
// client side of release tracking.
package release

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/helper/resourcespec"
)

// templateTypes maps the extensions of resource templates to the release
// resource types. Jobs keep the original ".nomad.tpl" extension.
var templateTypes = []struct {
	suffix string
	typ    string
}{
	{".namespace.hcl.tpl", api.ReleaseResourceNamespace},
	{".quota.hcl.tpl", api.ReleaseResourceQuota},
	{".acl.hcl.tpl", api.ReleaseResourceACLPolicy},
	{".var.hcl.tpl", api.ReleaseResourceVariable},
	{".kind.hcl.tpl", api.ReleaseResourceObjectKind},
	{".admission.hcl.tpl", api.ReleaseResourceAdmissionPolicy},
	{".netpol.hcl.tpl", resourceNetworkRule},
	{".object.hcl.tpl", api.ReleaseResourceObject},
	{".nomad.tpl", api.ReleaseResourceJob},
}

// resourceNetworkRule is reserved: network rules don't exist on the
// servers yet.
const resourceNetworkRule = "network-rule"

// TemplateType returns the release resource type of a template file, or ""
// if the file is not a resource template.
func TemplateType(name string) string {
	for _, t := range templateTypes {
		if strings.HasSuffix(name, t.suffix) {
			return t.typ
		}
	}
	return ""
}

// IsResourceTemplate reports whether a template file renders a release
// resource other than a job.
func IsResourceTemplate(name string) bool {
	t := TemplateType(name)
	return t != "" && t != api.ReleaseResourceJob
}

// ParseResource parses a rendered resource template other than a job.
func ParseResource(name, content string) (*api.ReleaseResource, error) {
	typ := TemplateType(name)
	res := &api.ReleaseResource{Type: typ, Source: name}
	file := strings.TrimSuffix(path.Base(name), ".tpl")
	b := []byte(content)
	var err error
	switch typ {
	case api.ReleaseResourceNamespace:
		res.Namespace, err = resourcespec.ParseNamespace(b)
	case api.ReleaseResourceQuota:
		res.Quota, err = resourcespec.ParseQuota(b)
	case api.ReleaseResourceACLPolicy:
		res.ACLPolicy, err = resourcespec.ParseACLPolicy(file, b)
	case api.ReleaseResourceVariable:
		res.Variable, res.SecretKeys, err = resourcespec.ParseVariableWithSecrets(b)
	case api.ReleaseResourceObjectKind:
		res.ObjectKind, err = resourcespec.ParseObjectKind(file, b, false)
	case api.ReleaseResourceAdmissionPolicy:
		res.AdmissionPolicy, err = resourcespec.ParseAdmissionPolicy(file, b)
	case api.ReleaseResourceObject:
		res.Object, err = resourcespec.ParseCustomObject(file, b, false)
	case resourceNetworkRule:
		return nil, fmt.Errorf("%s: network rules are not supported by the servers yet", name)
	default:
		return nil, fmt.Errorf("%s is not a resource template", name)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return res, nil
}

// SortResources sorts resources in the application order of the servers
// (by type order, then name), for display.
func SortResources(resources []*api.ReleaseResource) {
	order := map[string]int{}
	for i, t := range templateTypes {
		order[t.typ] = i
	}
	sort.SliceStable(resources, func(i, j int) bool {
		return order[resources[i].Type] < order[resources[j].Type]
	})
}

// validReleaseName is the rule of release names (custom object names).
var validReleaseName = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9._-]{0,98}[a-zA-Z0-9])?$`)

// invalidNameChars are replaced when a release name is derived from a
// deployment name such as "redis@latest".
var invalidNameChars = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// Name returns the release name for a deployment name: the deployment name
// with characters that are not allowed in release names replaced by "-".
func Name(deploymentName string) (string, error) {
	name := strings.Trim(invalidNameChars.ReplaceAllString(deploymentName, "-"), "-._")
	if !validReleaseName.MatchString(name) {
		return "", fmt.Errorf("%q is not a valid release name: use letters, digits, '.', '_' and '-' (at most 100 characters)", deploymentName)
	}
	return name, nil
}

// secretLike matches variable names whose values are not recorded in the
// release.
var secretLike = regexp.MustCompile(`(?i)(password|passwd|secret|token|credential|private|api_?key|access_?key)`)

// Redacted is the value recorded for variables whose name looks secret.
const Redacted = "(redacted)"

// Values returns the variable overrides to record in a release, with the
// values of secret-looking variables redacted, and the hash of all
// overrides (including the redacted values and the variable files).
func Values(vars map[string]string, varFiles map[string][]byte) (map[string]any, string) {
	values := make(map[string]any, len(vars))
	for k, v := range vars {
		if secretLike.MatchString(k) {
			values[k] = Redacted
		} else {
			values[k] = v
		}
	}
	h := sha256.New()
	b, _ := json.Marshal(vars)
	h.Write(b)
	names := make([]string, 0, len(varFiles))
	for n := range varFiles {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		h.Write([]byte("\x00" + n + "\x00"))
		h.Write(varFiles[n])
	}
	return values, "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// ParseSecret parses a -var-secret value: "<variable path>:<item>=<value>".
func ParseSecret(s string) (path, item, value string, err error) {
	path, rest, ok := strings.Cut(s, ":")
	if !ok || path == "" {
		return "", "", "", fmt.Errorf("invalid secret %q, must be <variable path>:<item>=<value>", s)
	}
	item, value, ok = strings.Cut(rest, "=")
	if !ok || item == "" {
		return "", "", "", fmt.Errorf("invalid secret %q, must be <variable path>:<item>=<value>", s)
	}
	return path, item, value, nil
}

// Secrets groups -var-secret values by Variables path.
func Secrets(flags []string) ([]*api.ReleaseVariableSecret, error) {
	byPath := map[string]*api.ReleaseVariableSecret{}
	var out []*api.ReleaseVariableSecret
	for _, f := range flags {
		p, item, value, err := ParseSecret(f)
		if err != nil {
			return nil, err
		}
		s, ok := byPath[p]
		if !ok {
			s = &api.ReleaseVariableSecret{Path: p, Items: map[string]string{}}
			byPath[p] = s
			out = append(out, s)
		}
		s.Items[item] = value
	}
	return out, nil
}
