// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package resourcespec

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/go-cty-funcs/filesystem"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/nomad/api"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
	"github.com/zclconf/go-cty/cty/function/stdlib"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

// isJSONInput reports whether the input is in JSON, either because jsonFlag
// is set or because the file has the .json extension.
func isJSONInput(path string, jsonFlag bool) bool {
	return jsonFlag || strings.HasSuffix(strings.ToLower(path), ".json")
}

// objectEvalContext returns the HCL functions available in specification
// files: file() reads a file relative to the specification, jsonencode()
// and jsondecode() convert values.
func objectEvalContext(path string) *hcl.EvalContext {
	baseDir := filepath.Dir(path)
	if path == "stdin.hcl" {
		baseDir, _ = os.Getwd()
	}
	return &hcl.EvalContext{
		Functions: map[string]function.Function{
			"file":       filesystem.MakeFileFunc(baseDir, false),
			"jsonencode": stdlib.JSONEncodeFunc,
			"jsondecode": stdlib.JSONDecodeFunc,
		},
	}
}

// objectKindHCL is the HCL form of an object kind:
//
//	kind "PostgresCluster" {
//	  group      = "db.example.com"
//	  namespaced = true
//	  schema     = file("postgrescluster.schema.json")
//	  rule { expression = "..." message = "..." }
//	  columns { column "Version" { path = "spec.version" } }
//	  scale { spec_path = "spec.replicas" status_path = "status.replicas" }
//	}
type objectKindHCL struct {
	Kind objectKindBodyHCL `hcl:"kind,block"`
}

type objectKindBodyHCL struct {
	Name        string                  `hcl:"name,label"`
	Group       string                  `hcl:"group"`
	Description string                  `hcl:"description,optional"`
	Namespaced  *bool                   `hcl:"namespaced,optional"`
	Schema      string                  `hcl:"schema"`
	Rules       []*objectKindRuleHCL    `hcl:"rule,block"`
	Columns     *objectKindColumnsHCL   `hcl:"columns,block"`
	Scale       *objectKindScaleHCL     `hcl:"scale,block"`
	Replicated  bool                    `hcl:"replicated,optional"`
	Version     uint64                  `hcl:"version,optional"`
	Migration   *objectKindMigrationHCL `hcl:"migration,block"`
	Webhooks    []*objectKindWebhookHCL `hcl:"webhook,block"`
	Limits      *objectKindLimitsHCL    `hcl:"limits,block"`
}

// objectKindMigrationHCL converts objects stored at older schema versions:
//
//	migration {
//	  spec   = "self.spec.omit(['port']).merge({'ports': [self.spec.port]})"
//	  status = "{}"
//	}
type objectKindMigrationHCL struct {
	Spec   string `hcl:"spec,optional"`
	Status string `hcl:"status,optional"`
}

// objectKindWebhookHCL is a type-bound admission webhook:
//
//	webhook "validate" {
//	  mode           = "validate"   # or "default"
//	  url            = "https://lb-webhook.service.consul:8443/validate"
//	  ca_cert        = file("ca.pem")
//	  timeout        = "5s"
//	  failure_policy = "fail"       # or "ignore"
//	  operations     = ["create", "update"]
//	}
type objectKindWebhookHCL struct {
	Name          string   `hcl:"name,label"`
	Mode          string   `hcl:"mode,optional"`
	URL           string   `hcl:"url"`
	CACert        string   `hcl:"ca_cert,optional"`
	TLSServerName string   `hcl:"tls_server_name,optional"`
	Timeout       string   `hcl:"timeout,optional"`
	FailurePolicy string   `hcl:"failure_policy,optional"`
	Operations    []string `hcl:"operations,optional"`
}

// objectKindLimitsHCL is the load management of the objects of a kind.
type objectKindLimitsHCL struct {
	WriteRate              float64 `hcl:"write_rate,optional"`
	WriteBurst             int     `hcl:"write_burst,optional"`
	IdentityWriteRate      float64 `hcl:"identity_write_rate,optional"`
	IdentityWriteBurst     int     `hcl:"identity_write_burst,optional"`
	MaxObjectsPerNamespace int     `hcl:"max_objects_per_namespace,optional"`
	MaxWatchesPerIdentity  int     `hcl:"max_watches_per_identity,optional"`
}

type objectKindRuleHCL struct {
	Expression string `hcl:"expression"`
	Message    string `hcl:"message,optional"`
}

type objectKindColumnsHCL struct {
	Columns []*objectKindColumnHCL `hcl:"column,block"`
}

type objectKindColumnHCL struct {
	Name string `hcl:"name,label"`
	Path string `hcl:"path"`
}

type objectKindScaleHCL struct {
	SpecPath   string `hcl:"spec_path"`
	StatusPath string `hcl:"status_path,optional"`
}

// ParseObjectKind parses an object kind in HCL or, for JSON input, in the
// API JSON form. In HCL, kinds are namespaced unless namespaced = false.
func ParseObjectKind(path string, content []byte, jsonInput bool) (*api.ObjectKind, error) {
	if isJSONInput(path, jsonInput) {
		var kind api.ObjectKind
		if err := json.Unmarshal(content, &kind); err != nil {
			return nil, err
		}
		return &kind, nil
	}

	file, diags := hclsyntax.ParseConfig(content, path, hcl.InitialPos)
	if diags.HasErrors() {
		return nil, diags
	}
	var spec objectKindHCL
	if diags := gohcl.DecodeBody(file.Body, objectEvalContext(path), &spec); diags.HasErrors() {
		return nil, diags
	}

	body := spec.Kind
	kind := &api.ObjectKind{
		Group:       body.Group,
		Kind:        body.Name,
		Description: body.Description,
		Namespaced:  body.Namespaced == nil || *body.Namespaced,
		Schema:      body.Schema,
		Replicated:  body.Replicated,
	}
	for _, r := range body.Rules {
		kind.Rules = append(kind.Rules, &api.ObjectKindRule{Expression: r.Expression, Message: r.Message})
	}
	if body.Columns != nil {
		for _, col := range body.Columns.Columns {
			kind.Columns = append(kind.Columns, &api.ObjectKindColumn{Name: col.Name, Path: col.Path})
		}
	}
	if body.Scale != nil {
		kind.Scale = &api.ObjectKindScale{SpecPath: body.Scale.SpecPath, StatusPath: body.Scale.StatusPath}
	}
	kind.Version = body.Version
	if m := body.Migration; m != nil {
		kind.Migration = &api.ObjectKindMigration{Spec: m.Spec, Status: m.Status}
	}
	for _, w := range body.Webhooks {
		hook := &api.ObjectKindWebhook{
			Name:          w.Name,
			Mode:          w.Mode,
			URL:           w.URL,
			CACert:        w.CACert,
			TLSServerName: w.TLSServerName,
			FailurePolicy: w.FailurePolicy,
			Operations:    w.Operations,
		}
		if w.Timeout != "" {
			d, err := time.ParseDuration(w.Timeout)
			if err != nil {
				return nil, fmt.Errorf("webhook %q: invalid timeout: %w", w.Name, err)
			}
			hook.Timeout = d
		}
		kind.Webhooks = append(kind.Webhooks, hook)
	}
	if l := body.Limits; l != nil {
		kind.Limits = &api.ObjectKindLimits{
			WriteRate:              l.WriteRate,
			WriteBurst:             l.WriteBurst,
			IdentityWriteRate:      l.IdentityWriteRate,
			IdentityWriteBurst:     l.IdentityWriteBurst,
			MaxObjectsPerNamespace: l.MaxObjectsPerNamespace,
			MaxWatchesPerIdentity:  l.MaxWatchesPerIdentity,
		}
	}
	return kind, nil
}

// ParseCustomObject parses an object in HCL or, for JSON input, in the API
// JSON form. The HCL form is:
//
//	kind      = "db.example.com/PostgresCluster"
//	name      = "billing"
//	namespace = "prod"
//	labels    = { team = "finance" }
//
//	spec {
//	  version  = "16"
//	  replicas = 3
//	  backup { schedule = "0 2 * * *" }
//	}
//
// Blocks in the spec become objects; a block type that repeats becomes a
// list and a labeled block becomes an object keyed by its label. The spec
// can also be given as an attribute: spec = { ... }.
func ParseCustomObject(path string, content []byte, jsonInput bool) (*api.CustomObject, error) {
	if isJSONInput(path, jsonInput) {
		var obj api.CustomObject
		dec := json.NewDecoder(strings.NewReader(string(content)))
		if err := dec.Decode(&obj); err != nil {
			return nil, err
		}
		return &obj, nil
	}

	file, diags := hclsyntax.ParseConfig(content, path, hcl.InitialPos)
	if diags.HasErrors() {
		return nil, diags
	}
	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return nil, fmt.Errorf("unexpected HCL body type %T", file.Body)
	}
	ctx := objectEvalContext(path)

	obj := &api.CustomObject{}
	for name, attr := range body.Attributes {
		val, diags := attr.Expr.Value(ctx)
		if diags.HasErrors() {
			return nil, diags
		}
		switch name {
		case "kind":
			ref, err := ctyString(name, val)
			if err != nil {
				return nil, err
			}
			group, kind, ok := strings.Cut(ref, "/")
			if !ok || group == "" || kind == "" {
				return nil, fmt.Errorf("invalid kind %q, must be <group>/<Kind>", ref)
			}
			obj.Group, obj.Kind = group, kind
		case "name":
			if obj.Name, diags = ctyStringDiag(name, val); diags != nil {
				return nil, diags
			}
		case "namespace":
			if obj.Namespace, diags = ctyStringDiag(name, val); diags != nil {
				return nil, diags
			}
		case "managed_by":
			if obj.ManagedBy, diags = ctyStringDiag(name, val); diags != nil {
				return nil, diags
			}
		case "labels":
			v, err := ctyToGo(val)
			if err != nil {
				return nil, fmt.Errorf("labels: %w", err)
			}
			m, ok := v.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("labels must be a map of strings")
			}
			obj.Labels = make(map[string]string, len(m))
			for k, e := range m {
				s, ok := e.(string)
				if !ok {
					return nil, fmt.Errorf("label %q must be a string", k)
				}
				obj.Labels[k] = s
			}
		case "spec":
			v, err := ctyToGo(val)
			if err != nil {
				return nil, fmt.Errorf("spec: %w", err)
			}
			m, ok := v.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("spec must be an object")
			}
			obj.Spec = m
		case "finalizers":
			v, err := ctyToGo(val)
			if err != nil {
				return nil, fmt.Errorf("finalizers: %w", err)
			}
			list, ok := v.([]any)
			if !ok {
				return nil, fmt.Errorf("finalizers must be a list of strings")
			}
			for _, e := range list {
				s, ok := e.(string)
				if !ok {
					return nil, fmt.Errorf("finalizers must be a list of strings")
				}
				obj.Finalizers = append(obj.Finalizers, s)
			}
		default:
			return nil, fmt.Errorf("%s: unsupported attribute %q", attr.NameRange, name)
		}
	}

	for _, block := range body.Blocks {
		if block.Type == "owner" && len(block.Labels) == 0 {
			m, err := hclBodyToMap(block.Body, ctx)
			if err != nil {
				return nil, err
			}
			ref := &api.ObjectOwnerReference{}
			for k, v := range m {
				s, ok := v.(string)
				if !ok {
					return nil, fmt.Errorf("%s: owner %s must be a string", block.TypeRange, k)
				}
				switch k {
				case "kind":
					ref.Kind = s
				case "name":
					ref.Name = s
				case "namespace":
					ref.Namespace = s
				default:
					return nil, fmt.Errorf("%s: unsupported owner attribute %q", block.TypeRange, k)
				}
			}
			obj.Owners = append(obj.Owners, ref)
			continue
		}
		if block.Type != "spec" || len(block.Labels) != 0 {
			return nil, fmt.Errorf("%s: unsupported block %q", block.TypeRange, block.Type)
		}
		if obj.Spec != nil {
			return nil, fmt.Errorf("%s: spec is defined more than once", block.TypeRange)
		}
		m, err := hclBodyToMap(block.Body, ctx)
		if err != nil {
			return nil, err
		}
		obj.Spec = m
	}

	if obj.Group == "" || obj.Name == "" {
		return nil, fmt.Errorf("kind and name are required")
	}
	return obj, nil
}

func ctyString(name string, val cty.Value) (string, error) {
	if val.IsNull() || !val.IsKnown() || val.Type() != cty.String {
		return "", fmt.Errorf("%s must be a string", name)
	}
	return val.AsString(), nil
}

func ctyStringDiag(name string, val cty.Value) (string, hcl.Diagnostics) {
	s, err := ctyString(name, val)
	if err != nil {
		return "", hcl.Diagnostics{{Severity: hcl.DiagError, Summary: err.Error()}}
	}
	return s, nil
}

// ctyToGo converts an HCL value to the value encoding/json produces.
func ctyToGo(val cty.Value) (any, error) {
	b, err := ctyjson.Marshal(val, val.Type())
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// hclBodyToMap converts a block body without a schema to a map.
func hclBodyToMap(body *hclsyntax.Body, ctx *hcl.EvalContext) (map[string]any, error) {
	out := make(map[string]any)
	for name, attr := range body.Attributes {
		val, diags := attr.Expr.Value(ctx)
		if diags.HasErrors() {
			return nil, diags
		}
		v, err := ctyToGo(val)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", attr.NameRange, err)
		}
		out[name] = v
	}

	// Group the blocks by type, in source order.
	counts := make(map[string]int)
	for _, block := range body.Blocks {
		counts[block.Type]++
	}
	types := make([]string, 0, len(counts))
	for t := range counts {
		types = append(types, t)
	}
	sort.Strings(types)

	for _, t := range types {
		if _, ok := out[t]; ok {
			return nil, fmt.Errorf("%q is defined both as an attribute and as a block", t)
		}
		var list []any
		labeled := make(map[string]any)
		for _, block := range body.Blocks {
			if block.Type != t {
				continue
			}
			m, err := hclBodyToMap(block.Body, ctx)
			if err != nil {
				return nil, err
			}
			switch len(block.Labels) {
			case 0:
				list = append(list, m)
			case 1:
				if _, ok := labeled[block.Labels[0]]; ok {
					return nil, fmt.Errorf("%s: duplicate block %s %q", block.TypeRange, t, block.Labels[0])
				}
				labeled[block.Labels[0]] = m
			default:
				return nil, fmt.Errorf("%s: blocks may have at most one label", block.TypeRange)
			}
		}
		switch {
		case len(list) > 0 && len(labeled) > 0:
			return nil, fmt.Errorf("blocks %q mix labeled and unlabeled blocks", t)
		case len(labeled) > 0:
			out[t] = labeled
		case counts[t] == 1:
			out[t] = list[0]
		default:
			out[t] = list
		}
	}
	return out, nil
}
