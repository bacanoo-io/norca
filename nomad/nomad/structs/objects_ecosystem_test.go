// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"testing"
	"time"

	"github.com/shoenig/test/must"
)

func testEcoKind() *ObjectKind {
	return &ObjectKind{Group: "db.example.com", Kind: "PostgresCluster", Schema: `{}`}
}

func TestObjectKind_Canonicalize(t *testing.T) {
	k := testEcoKind()
	k.Webhooks = []*ObjectKindWebhook{{Name: "v", URL: "https://x.example.com/v"}}
	k.Limits = &ObjectKindLimits{WriteRate: 2.5, IdentityWriteRate: 1}
	k.Migration = &ObjectKindMigration{}
	k.Canonicalize()
	must.Eq(t, uint64(1), k.Version)
	w := k.Webhooks[0]
	must.Eq(t, ObjectWebhookModeValidate, w.Mode)
	must.Eq(t, ObjectWebhookFailurePolicyFail, w.FailurePolicy)
	must.Eq(t, DefaultObjectWebhookTimeout, w.Timeout)
	must.Eq(t, []string{ObjectWebhookOperationCreate, ObjectWebhookOperationUpdate}, w.Operations)
	must.Eq(t, 3, k.Limits.WriteBurst)
	must.Eq(t, 1, k.Limits.IdentityWriteBurst)
	must.Nil(t, k.Migration)
	must.NoError(t, k.Validate())
}

func TestObjectKind_ValidateEcosystem(t *testing.T) {
	cases := []struct {
		name   string
		modify func(k *ObjectKind)
		errStr string
	}{
		{"http url", func(k *ObjectKind) {
			k.Webhooks = []*ObjectKindWebhook{{Name: "v", URL: "http://x.example.com"}}
		}, "absolute https URL"},
		{"user info", func(k *ObjectKind) {
			k.Webhooks = []*ObjectKindWebhook{{Name: "v", URL: "https://u:p@x.example.com"}}
		}, "absolute https URL"},
		{"duplicate", func(k *ObjectKind) {
			k.Webhooks = []*ObjectKindWebhook{{Name: "v", URL: "https://a"}, {Name: "v", URL: "https://b"}}
		}, "duplicate name"},
		{"bad name", func(k *ObjectKind) {
			k.Webhooks = []*ObjectKindWebhook{{Name: "-v", URL: "https://a"}}
		}, "invalid name"},
		{"mode", func(k *ObjectKind) {
			k.Webhooks = []*ObjectKindWebhook{{Name: "v", Mode: "mutate", URL: "https://a"}}
		}, "invalid mode"},
		{"default reviews delete", func(k *ObjectKind) {
			k.Webhooks = []*ObjectKindWebhook{{Name: "v", Mode: ObjectWebhookModeDefault, URL: "https://a", Operations: []string{"delete"}}}
		}, "can't review"},
		{"operation", func(k *ObjectKind) {
			k.Webhooks = []*ObjectKindWebhook{{Name: "v", URL: "https://a", Operations: []string{"status"}}}
		}, "invalid operation"},
		{"timeout", func(k *ObjectKind) {
			k.Webhooks = []*ObjectKindWebhook{{Name: "v", URL: "https://a", Timeout: time.Minute}}
		}, "timeout"},
		{"failure policy", func(k *ObjectKind) {
			k.Webhooks = []*ObjectKindWebhook{{Name: "v", URL: "https://a", FailurePolicy: "maybe"}}
		}, "failure_policy"},
		{"ca", func(k *ObjectKind) {
			k.Webhooks = []*ObjectKindWebhook{{Name: "v", URL: "https://a", CACert: "not a pem"}}
		}, "PEM"},
		{"limits", func(k *ObjectKind) {
			k.Limits = &ObjectKindLimits{WriteRate: -1}
		}, "write_rate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := testEcoKind()
			tc.modify(k)
			must.ErrorContains(t, k.Validate(), tc.errStr)
		})
	}
}

func TestObjectKindWebhook_Approval(t *testing.T) {
	w := &ObjectKindWebhook{Name: "v", URL: "https://a/v", Timeout: time.Second}
	must.False(t, w.Approved())
	w.Approval = &ObjectKindWebhookApproval{Digest: w.Digest(), ApprovedBy: "token:x"}
	must.True(t, w.Approved())

	// The digest ignores the approval, and covers the configuration.
	c := w.Copy()
	must.Eq(t, w.Digest(), c.Digest())
	c.Approval = nil
	must.Eq(t, w.Digest(), c.Digest())
	c.CACert = "-----BEGIN CERTIFICATE-----"
	must.NotEq(t, w.Digest(), c.Digest())
	w.FailurePolicy = ObjectWebhookFailurePolicyIgnore
	must.False(t, w.Approved())

	k := testEcoKind()
	k.Webhooks = []*ObjectKindWebhook{w}
	stripped := k.WithoutApprovals()
	must.Nil(t, stripped.Webhooks[0].Approval)
	must.NotNil(t, k.Webhooks[0].Approval)
	must.Eq(t, w, k.Webhook("v"))
	must.Nil(t, k.Webhook("x"))

	// Copy is deep.
	kc := k.Copy()
	kc.Webhooks[0].Approval.Digest = "x"
	kc.Webhooks[0].Operations = append(kc.Webhooks[0].Operations, "delete")
	must.NotEq(t, "x", k.Webhooks[0].Approval.Digest)
	must.SliceEmpty(t, k.Webhooks[0].Operations)
}

func TestObjectKindWebhook_Reviews(t *testing.T) {
	w := &ObjectKindWebhook{}
	must.True(t, w.Reviews(ObjectWebhookOperationCreate))
	must.False(t, w.Reviews(ObjectWebhookOperationDelete))
	w.Operations = []string{ObjectWebhookOperationDelete}
	must.True(t, w.Reviews(ObjectWebhookOperationDelete))
	must.False(t, w.Reviews(ObjectWebhookOperationCreate))
}

func TestSchemaVersions(t *testing.T) {
	must.Eq(t, uint64(1), (&ObjectKind{}).SchemaVersion())
	must.Eq(t, uint64(3), (&ObjectKind{Version: 3}).SchemaVersion())
	must.Eq(t, uint64(1), (&CustomObject{}).SchemaVersion())
	must.Eq(t, uint64(2), (&CustomObject{KindVersion: 2}).SchemaVersion())
	err := NewObjectLimitError("too %s", "much")
	code, msg, ok := CodeFromRPCCodedErr(err)
	must.True(t, ok)
	must.Eq(t, 429, code)
	must.StrContains(t, msg, "too much")
}
