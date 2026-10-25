// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package objectlint checks object kinds offline, without a server: it
// compiles the JSON Schema, the CEL rules and the migration of a kind like
// the servers do, adds advice on the conventions of the operator framework,
// and validates sample objects against the kind (defaults, schema, rules).
// It backs "norca object-kind lint" and the tests of scaffolded operators.
package objectlint

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/nomad/admission"
	"github.com/hashicorp/nomad/nomad/objectschema"
	"github.com/hashicorp/nomad/nomad/structs"
)

// Severity of a finding.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Finding is a problem found in a kind or a sample object.
type Finding struct {
	Severity Severity
	// Subject is what the finding is about: "kind", "schema", "rule 0",
	// "webhook \"x\"", "object default/web", ...
	Subject string
	Message string
}

func (f *Finding) String() string {
	return fmt.Sprintf("%s: %s: %s", f.Severity, f.Subject, f.Message)
}

// Report is the result of a lint run.
type Report struct {
	Findings []*Finding

	// Objects are the sample objects after the defaults of the schema were
	// filled in, in the order given.
	Objects []*api.CustomObject
}

// HasErrors reports whether the report has findings of severity error.
func (r *Report) HasErrors() bool {
	for _, f := range r.Findings {
		if f.Severity == SeverityError {
			return true
		}
	}
	return false
}

func (r *Report) add(sev Severity, subject, format string, args ...any) {
	r.Findings = append(r.Findings, &Finding{Severity: sev, Subject: subject, Message: fmt.Sprintf(format, args...)})
}

// ToStructs converts an API kind to the server form.
func ToStructs(kind *api.ObjectKind) (*structs.ObjectKind, error) {
	b, err := json.Marshal(kind)
	if err != nil {
		return nil, err
	}
	var out structs.ObjectKind
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Lint checks the kind and validates the sample objects against it.
func Lint(apiKind *api.ObjectKind, samples []*api.CustomObject) (*Report, error) {
	r := &Report{}
	kind, err := ToStructs(apiKind)
	if err != nil {
		return nil, err
	}
	kind.Canonicalize()

	if err := kind.Validate(); err != nil {
		for _, msg := range splitMultiError(err) {
			r.add(SeverityError, "kind", "%s", msg)
		}
	}
	schema, err := objectschema.Compile(kind.Group, kind.Kind, kind.Schema)
	if err != nil {
		r.add(SeverityError, "schema", "%v", err)
	}
	rules, err := admission.NewObjectRuleEngine()
	if err != nil {
		return nil, err
	}
	if err := rules.CompileRules(kind); err != nil {
		r.add(SeverityError, "rules", "%v", err)
	}
	migrations, err := admission.NewMigrationEngine()
	if err != nil {
		return nil, err
	}
	if err := migrations.Compile(kind.Migration); err != nil {
		r.add(SeverityError, "migration", "%v", err)
	}

	lintConventions(r, kind)

	if schema == nil || len(samples) == 0 {
		return r, nil
	}
	prepared := rules.Prepare(kind)
	for _, sample := range samples {
		lintSample(r, kind, schema, prepared, sample)
	}
	return r, nil
}

// lintConventions adds advice on the conventions of the framework.
func lintConventions(r *Report, kind *structs.ObjectKind) {
	if strings.TrimSpace(kind.Description) == "" {
		r.add(SeverityWarning, "kind", "no description; listings and the UI show it")
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(kind.Schema), &doc); err == nil {
		props, _ := doc["properties"].(map[string]any)
		if _, ok := props["spec"]; !ok {
			r.add(SeverityWarning, "schema", "the schema does not describe \"spec\"; objects are validated as {\"spec\": ..., \"status\": ...}")
		}
		status, _ := props["status"].(map[string]any)
		if status == nil {
			r.add(SeverityWarning, "schema", "the schema does not describe \"status\"; controllers report observedGeneration and conditions there")
		} else {
			sp, _ := status["properties"].(map[string]any)
			for _, field := range []string{structs.ObjectStatusObservedGeneration, structs.ObjectStatusConditions} {
				if _, ok := sp[field]; !ok && status["additionalProperties"] == false {
					r.add(SeverityError, "schema", "status does not allow %q (additionalProperties is false), which the controller SDK writes", field)
				}
			}
		}
	}
	if kind.Scale != nil && !strings.HasPrefix(kind.Scale.SpecPath, "spec.") {
		r.add(SeverityError, "scale", "spec_path must start with \"spec.\"")
	}
	for _, w := range kind.Webhooks {
		if w == nil {
			continue
		}
		subject := fmt.Sprintf("webhook %q", w.Name)
		if u, err := url.Parse(w.URL); err == nil && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1") {
			r.add(SeverityWarning, subject, "the URL points to the loopback interface of the leader, which is rarely where the webhook runs")
		}
		if w.CACert == "" {
			r.add(SeverityWarning, subject, "no ca_cert: the certificate must be signed by a CA the servers trust")
		}
		if w.IgnoreFailure() && w.Mode == structs.ObjectWebhookModeValidate {
			r.add(SeverityWarning, subject, "failure_policy \"ignore\" lets writes through unvalidated while the webhook is down")
		}
	}
	if kind.Migration != nil && kind.SchemaVersion() == 1 {
		r.add(SeverityWarning, "migration", "the migration only runs for objects stored at an older version; raise the version")
	}
	if l := kind.Limits; l != nil && l.IdentityWriteRate > 0 && l.WriteRate > 0 && l.IdentityWriteRate > l.WriteRate {
		r.add(SeverityWarning, "limits", "identity_write_rate is higher than write_rate, so it never applies")
	}
}

// lintSample fills in the defaults of a sample object and validates it.
func lintSample(r *Report, kind *structs.ObjectKind, schema *objectschema.Schema, rules *admission.PreparedRules, sample *api.CustomObject) {
	subject := "object " + sample.Name
	if sample.Namespace != "" {
		subject = "object " + sample.Namespace + "/" + sample.Name
	}
	if sample.Group != "" && (sample.Group != kind.Group || sample.Kind != kind.Kind) {
		r.add(SeverityError, subject, "is of kind %s/%s, not %s", sample.Group, sample.Kind, kind.KindRef())
		return
	}
	obj := &structs.CustomObject{
		Group: kind.Group, Kind: kind.Kind, Namespace: sample.Namespace, Name: sample.Name,
		Labels: sample.Labels, ManagedBy: sample.ManagedBy, Finalizers: sample.Finalizers, Generation: 1,
		KindVersion: kind.SchemaVersion(),
	}
	if err := obj.ValidateMetadata(); err != nil {
		for _, msg := range splitMultiError(err) {
			r.add(SeverityError, subject, "%s", msg)
		}
	}
	spec, err := structs.NormalizeObjectMap(sample.Spec)
	if err != nil {
		r.add(SeverityError, subject, "spec can't be encoded as JSON: %v", err)
		return
	}
	if spec == nil {
		spec = map[string]any{}
	}
	doc := map[string]any{"spec": spec}
	schema.ApplyDefaults(doc)
	obj.Spec, _ = doc["spec"].(map[string]any)
	if err := schema.Validate(map[string]any{"spec": obj.Spec}); err != nil {
		r.add(SeverityError, subject, "%v", err)
	}

	self, err := admission.ObjectValue(map[string]any{
		"kind": kind.KindRef(),
		"metadata": map[string]any{
			"name": obj.Name, "namespace": obj.Namespace, "labels": nonNilLabels(obj.Labels),
			"managedBy": obj.ManagedBy, "generation": 1, "createIndex": 0, "modifyIndex": 0,
			"finalizers": nonNilStrings(obj.Finalizers), "deleting": false, "kindVersion": kind.SchemaVersion(),
		},
		"spec": obj.Spec,
	})
	if err == nil {
		request := map[string]any{
			"operation": "create", "namespace": obj.Namespace, "dryRun": true,
			"user": map[string]any{"accessor_id": "", "name": "", "type": "anonymous", "roles": []string{}, "policies": []string{}},
		}
		for _, v := range rules.Evaluate(context.Background(), self, nil, request) {
			r.add(SeverityError, subject, "%s", v.Message)
		}
	}

	out := *sample
	out.Group, out.Kind = kind.Group, kind.Kind
	out.Spec = obj.Spec
	r.Objects = append(r.Objects, &out)
}

func nonNilLabels(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// splitMultiError returns the messages of a (multi)error, one per line.
func splitMultiError(err error) []string {
	var out []string
	for _, line := range strings.Split(err.Error(), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "*"))
		if line == "" || strings.HasSuffix(line, "errors occurred:") || strings.HasSuffix(line, "error occurred:") {
			continue
		}
		out = append(out, line)
	}
	return out
}
