// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package config

import (
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
)

func TestAdmissionConfig_CanonicalizeValidate(t *testing.T) {
	ci.Parallel(t)

	cfg := &AdmissionConfig{Webhooks: []*AdmissionWebhookConfig{{Name: "a", URL: "https://x:8443/validate"}}}
	cfg.Canonicalize()
	must.Eq(t, AdmissionWebhookModeValidate, cfg.Webhooks[0].Mode)
	must.Eq(t, AdmissionFailurePolicyFail, cfg.Webhooks[0].FailurePolicy)
	must.Eq(t, 5*time.Second, cfg.Webhooks[0].Timeout)
	must.NoError(t, cfg.Validate())

	bad := []*AdmissionWebhookConfig{
		{Name: "", URL: "https://x"},
		{Name: "a", URL: "ftp://x"},
		{Name: "a", URL: "https://x", Mode: "audit"},
		{Name: "a", URL: "https://x", FailurePolicy: "retry"},
		{Name: "a", URL: "https://x", CertFile: "c.pem"},
	}
	for _, w := range bad {
		c := &AdmissionConfig{Webhooks: []*AdmissionWebhookConfig{w}}
		c.Canonicalize()
		must.Error(t, c.Validate())
	}

	dup := &AdmissionConfig{Webhooks: []*AdmissionWebhookConfig{
		{Name: "a", URL: "https://x"}, {Name: "a", URL: "https://y"},
	}}
	dup.Canonicalize()
	must.ErrorContains(t, dup.Validate(), "duplicate")
}

func TestAdmissionConfig_Merge(t *testing.T) {
	ci.Parallel(t)

	a := &AdmissionConfig{Webhooks: []*AdmissionWebhookConfig{
		{Name: "one", URL: "https://1"}, {Name: "two", URL: "https://2"},
	}}
	b := &AdmissionConfig{Webhooks: []*AdmissionWebhookConfig{
		{Name: "two", URL: "https://2b"}, {Name: "three", URL: "https://3"},
	}}
	m := a.Merge(b)
	must.Len(t, 3, m.Webhooks)
	must.Eq(t, "https://1", m.Webhooks[0].URL)
	must.Eq(t, "https://2b", m.Webhooks[1].URL)
	must.Eq(t, "three", m.Webhooks[2].Name)
	must.Eq(t, "https://2", a.Webhooks[1].URL)

	var nilCfg *AdmissionConfig
	must.Eq(t, b, nilCfg.Merge(b))
}
