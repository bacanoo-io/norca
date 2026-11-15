// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// The kinds of certificates: who signs (CertificateIssuer), what is kept
// issued (Certificate) and one issuance handed to an external issuer
// (CertificateOrder). They are registered by the servers, like the kinds of
// node scaling, and their objects are written through the generic object
// API, with the checks of certificate_checks.go.
//
// Object events are published as they are, so nothing in a spec or a status
// of these kinds may be a secret: the keys are in the secret store, an order
// holds a signing request and a public chain.

const certificateConditions = `{"type": "array", "items": {"type": "object"}}`

// certificateIssuerKindSchema is the JSON Schema of issuers. The status of
// an external issuer is written by its controller: the heartbeat and the
// roots its chains lead to.
const certificateIssuerKindSchema = `{
  "type": "object",
  "properties": {
    "spec": {
      "type": "object",
      "properties": {
        "builtin": {"type": "object", "additionalProperties": false},
        "external": {
          "type": "object",
          "required": ["controller"],
          "properties": {
            "controller": {"type": "string", "minLength": 1, "maxLength": 128},
            "config": {"type": "object"}
          },
          "additionalProperties": false
        }
      },
      "additionalProperties": false
    },
    "status": {
      "type": "object",
      "properties": {
        "heartbeat": {"type": "string"},
        "version": {"type": "string"},
        "roots": {"type": "string"},
        "authority": {"type": "string"},
        "notAfter": {"type": "string"},
        "conditions": ` + certificateConditions + `,
        "observedGeneration": {"type": "integer", "minimum": 0}
      }
    }
  }
}`

// certificateKindSchema is the JSON Schema of certificates. The status is
// written by the servers only.
const certificateKindSchema = `{
  "type": "object",
  "properties": {
    "spec": {
      "type": "object",
      "required": ["issuer", "secret_path"],
      "properties": {
        "issuer": {"type": "string", "minLength": 1},
        "dns_names": {"type": "array", "maxItems": 100, "items": {"type": "string", "minLength": 1, "maxLength": 253}},
        "ip_addresses": {"type": "array", "maxItems": 100, "items": {"type": "string", "minLength": 1}},
        "secret_path": {"type": "string", "minLength": 1},
        "validity": {"type": "string"},
        "renew_before": {"type": "string"},
        "key_type": {"type": "string", "enum": ["ecdsa-p256", "ecdsa-p384", "rsa-2048", "rsa-4096", "ed25519"], "default": "ecdsa-p256"},
        "delete_secret": {"type": "boolean", "default": false}
      },
      "additionalProperties": false
    },
    "status": {
      "type": "object",
      "properties": {
        "issuer": {"type": "string"},
        "authority": {"type": "string"},
        "serial": {"type": "string"},
        "notBefore": {"type": "string"},
        "notAfter": {"type": "string"},
        "renewAt": {"type": "string"},
        "secretVersion": {"type": "integer", "minimum": 0},
        "request": {"type": "string"},
        "order": {"type": "string"},
        "orderRequest": {"type": "string"},
        "orders": {"type": "integer", "minimum": 0},
        "revoke": {"type": "string"},
        "failures": {"type": "integer", "minimum": 0},
        "retryAt": {"type": "string"},
        "conditions": ` + certificateConditions + `,
        "observedGeneration": {"type": "integer", "minimum": 0}
      },
      "additionalProperties": false
    }
  }
}`

// certificateOrderKindSchema is the JSON Schema of orders: the spec written
// by the servers, the status by the controller of the issuer.
const certificateOrderKindSchema = `{
  "type": "object",
  "properties": {
    "spec": {
      "type": "object",
      "required": ["issuer", "certificate"],
      "properties": {
        "issuer": {"type": "string", "minLength": 1},
        "certificate": {"type": "string", "minLength": 1},
        "csr": {"type": "string"},
        "dns_names": {"type": "array", "items": {"type": "string"}},
        "revoke": {"type": "string"}
      },
      "additionalProperties": false
    },
    "status": {
      "type": "object",
      "properties": {
        "state": {"type": "string", "enum": ["pending", "processing", "valid", "failed"]},
        "chain": {"type": "string"},
        "renew_after": {"type": "string"},
        "problem": {"type": "string"},
        "progress": {"type": "object"},
        "conditions": ` + certificateConditions + `,
        "observedGeneration": {"type": "integer", "minimum": 0}
      }
    }
  }
}`

// builtinCertificateKinds returns the kinds of certificates.
func builtinCertificateKinds() []*structs.ObjectKind {
	return []*structs.ObjectKind{
		{
			Group:       structs.OperatorsGroup,
			Kind:        structs.CertificateIssuerKind,
			Description: "Who signs certificates: the issuing CA of the region, or an external issuer fulfilled by a controller (built in)",
			Namespaced:  false,
			Schema:      certificateIssuerKindSchema,
			Rules: []*structs.ObjectKindRule{
				{
					Expression: "has(self.spec.builtin) != has(self.spec.external)",
					Message:    "an issuer is either builtin or external",
				},
				{
					Expression: "!has(oldSelf) || has(self.spec.builtin) == has(oldSelf.spec.builtin)",
					Message:    "a built-in issuer can't become an external one, nor the other way: delete it and create it again",
				},
			},
			Columns: []*structs.ObjectKindColumn{
				{Name: "Controller", Path: "spec.external.controller"},
				{Name: "Heartbeat", Path: "status.heartbeat"},
				{Name: "Authority", Path: "status.authority"},
			},
		},
		{
			Group:       structs.OperatorsGroup,
			Kind:        structs.CertificateKind,
			Description: "TLS certificate for DNS names, kept issued and renewed in the secret store (built in)",
			Namespaced:  true,
			Schema:      certificateKindSchema,
			Rules: []*structs.ObjectKindRule{
				{
					Expression: "!has(oldSelf) || self.spec.secret_path == oldSelf.spec.secret_path",
					Message:    "secret_path can't be changed: delete the certificate and create it again",
				},
			},
			Columns: []*structs.ObjectKindColumn{
				{Name: "Issuer", Path: "spec.issuer"},
				{Name: "Secret", Path: "spec.secret_path"},
				{Name: "Not After", Path: "status.notAfter"},
				{Name: "Renew At", Path: "status.renewAt"},
				{Name: "Serial", Path: "status.serial"},
			},
		},
		{
			Group:       structs.OperatorsGroup,
			Kind:        structs.CertificateOrderKind,
			Description: "One issuance handed to an external issuer: the signing request in, the chain out (built in, written by the servers)",
			Namespaced:  false,
			Schema:      certificateOrderKindSchema,
			Columns: []*structs.ObjectKindColumn{
				{Name: "Issuer", Path: "spec.issuer"},
				{Name: "Certificate", Path: "spec.certificate"},
				{Name: "State", Path: "status.state"},
				{Name: "Problem", Path: "status.problem"},
			},
		},
	}
}

// isCertificateKind reports whether the kind is one of the kinds of
// certificates.
func isCertificateKind(group, kind string) bool {
	if group != structs.OperatorsGroup {
		return false
	}
	switch kind {
	case structs.CertificateIssuerKind, structs.CertificateKind, structs.CertificateOrderKind:
		return true
	}
	return false
}
