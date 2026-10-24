// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package config

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"time"

	"github.com/hashicorp/go-multierror"
)

const (
	// AdmissionWebhookModeValidate webhooks can only accept or reject a job.
	AdmissionWebhookModeValidate = "validate"

	// AdmissionWebhookModeMutate webhooks can also modify a job with a JSON
	// patch.
	AdmissionWebhookModeMutate = "mutate"

	// AdmissionFailurePolicyFail rejects the job when the webhook cannot be
	// called or returns an invalid response.
	AdmissionFailurePolicyFail = "fail"

	// AdmissionFailurePolicyIgnore admits the job with a warning when the
	// webhook cannot be called or returns an invalid response.
	AdmissionFailurePolicyIgnore = "ignore"

	// DefaultAdmissionWebhookTimeout is the default timeout of a webhook call.
	DefaultAdmissionWebhookTimeout = 5 * time.Second

	// AdmissionResourceJob and AdmissionResourceVariable are the object
	// kinds a webhook can be sent. They match the structs constants of the
	// same name.
	AdmissionResourceJob      = "job"
	AdmissionResourceVariable = "variable"
)

// AdmissionConfig is the server configuration of dynamic admission control.
// Webhooks can only be configured here and never through the API, so that a
// token cannot register a webhook that would see every job.
type AdmissionConfig struct {
	// Webhooks are called in the configured order.
	Webhooks []*AdmissionWebhookConfig `hcl:"webhook"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

// AdmissionWebhookConfig configures a single admission webhook.
type AdmissionWebhookConfig struct {
	// Name is the unique name of the webhook.
	Name string `hcl:",key"`

	// URL is the http(s) URL the admission review is POSTed to.
	URL string `hcl:"url"`

	// Mode is "validate" (default) or "mutate".
	Mode string `hcl:"mode"`

	// Timeout of a single call, default 5s.
	Timeout    time.Duration `hcl:"-"`
	TimeoutHCL string        `hcl:"timeout" json:"-"`

	// FailurePolicy is "fail" (default) or "ignore".
	FailurePolicy string `hcl:"failure_policy"`

	// Namespaces restricts the webhook to these namespaces (glob patterns
	// allowed). Empty means every namespace.
	Namespaces []string `hcl:"namespaces"`

	// ExcludeNamespaces are never sent to the webhook (glob patterns
	// allowed). Use it for the namespace the webhook service itself runs in.
	ExcludeNamespaces []string `hcl:"exclude_namespaces"`

	// Resources is the list of object kinds sent to the webhook: "job"
	// and/or "variable". Empty means jobs only. Mutating webhooks only apply
	// to jobs.
	Resources []string `hcl:"resources"`

	// VariablePaths restricts the Variables writes sent to the webhook
	// (glob patterns allowed). Empty means every path.
	VariablePaths []string `hcl:"variable_paths"`

	// IncludeVariableItems sends the decrypted items of Variables (the
	// written and the previous ones) to the webhook. By default the webhook
	// only receives their namespace, path and indexes.
	IncludeVariableItems bool `hcl:"include_variable_items"`

	// CAFile is the PEM CA bundle used to verify the webhook's certificate.
	// The system roots are used when it is empty.
	CAFile string `hcl:"ca_file"`

	// CertFile and KeyFile are an optional client certificate for mTLS.
	CertFile string `hcl:"cert_file"`
	KeyFile  string `hcl:"key_file"`

	// TLSServerName overrides the server name used to verify the webhook's
	// certificate.
	TLSServerName string `hcl:"tls_server_name"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

// Canonicalize sets the defaults of every webhook.
func (a *AdmissionConfig) Canonicalize() {
	if a == nil {
		return
	}
	for _, w := range a.Webhooks {
		if w.Mode == "" {
			w.Mode = AdmissionWebhookModeValidate
		}
		if w.FailurePolicy == "" {
			w.FailurePolicy = AdmissionFailurePolicyFail
		}
		if w.Timeout == 0 {
			w.Timeout = DefaultAdmissionWebhookTimeout
		}
	}
}

// Validate returns an error if the configuration is invalid. It expects a
// canonicalized configuration.
func (a *AdmissionConfig) Validate() error {
	if a == nil {
		return nil
	}

	var mErr *multierror.Error
	seen := make(map[string]struct{}, len(a.Webhooks))
	for _, w := range a.Webhooks {
		if w.Name == "" {
			mErr = multierror.Append(mErr, errors.New("admission webhook without name"))
			continue
		}
		if _, ok := seen[w.Name]; ok {
			mErr = multierror.Append(mErr, fmt.Errorf("admission webhook %q: duplicate name", w.Name))
		}
		seen[w.Name] = struct{}{}

		if u, err := url.Parse(w.URL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			mErr = multierror.Append(mErr, fmt.Errorf("admission webhook %q: url must be an absolute http(s) URL", w.Name))
		}
		switch w.Mode {
		case AdmissionWebhookModeValidate, AdmissionWebhookModeMutate:
		default:
			mErr = multierror.Append(mErr, fmt.Errorf("admission webhook %q: invalid mode %q", w.Name, w.Mode))
		}
		switch w.FailurePolicy {
		case AdmissionFailurePolicyFail, AdmissionFailurePolicyIgnore:
		default:
			mErr = multierror.Append(mErr, fmt.Errorf("admission webhook %q: invalid failure_policy %q", w.Name, w.FailurePolicy))
		}
		if w.Timeout < 0 {
			mErr = multierror.Append(mErr, fmt.Errorf("admission webhook %q: timeout must be positive", w.Name))
		}
		if (w.CertFile == "") != (w.KeyFile == "") {
			mErr = multierror.Append(mErr, fmt.Errorf("admission webhook %q: cert_file and key_file must be set together", w.Name))
		}
		forVariables := false
		for _, r := range w.Resources {
			switch r {
			case AdmissionResourceJob:
			case AdmissionResourceVariable:
				forVariables = true
			default:
				mErr = multierror.Append(mErr, fmt.Errorf("admission webhook %q: invalid resource %q", w.Name, r))
			}
		}
		if forVariables && w.Mode == AdmissionWebhookModeMutate {
			mErr = multierror.Append(mErr, fmt.Errorf("admission webhook %q: mutating webhooks cannot apply to the %q resource", w.Name, AdmissionResourceVariable))
		}
		if !forVariables && (len(w.VariablePaths) > 0 || w.IncludeVariableItems) {
			mErr = multierror.Append(mErr, fmt.Errorf("admission webhook %q: variable_paths and include_variable_items require the %q resource", w.Name, AdmissionResourceVariable))
		}
	}
	return mErr.ErrorOrNil()
}

// Copy returns a deep copy of the configuration.
func (a *AdmissionConfig) Copy() *AdmissionConfig {
	if a == nil {
		return nil
	}
	nc := new(AdmissionConfig)
	*nc = *a
	nc.ExtraKeysHCL = slices.Clone(a.ExtraKeysHCL)
	if a.Webhooks != nil {
		nc.Webhooks = make([]*AdmissionWebhookConfig, len(a.Webhooks))
		for i, w := range a.Webhooks {
			nc.Webhooks[i] = w.Copy()
		}
	}
	return nc
}

// Merge merges two configurations. Webhooks of b replace the webhooks of a
// with the same name, new webhooks are appended.
func (a *AdmissionConfig) Merge(b *AdmissionConfig) *AdmissionConfig {
	if a == nil {
		return b.Copy()
	}
	result := a.Copy()
	if b == nil {
		return result
	}
	for _, w := range b.Webhooks {
		idx := slices.IndexFunc(result.Webhooks, func(x *AdmissionWebhookConfig) bool { return x.Name == w.Name })
		if idx >= 0 {
			result.Webhooks[idx] = w.Copy()
		} else {
			result.Webhooks = append(result.Webhooks, w.Copy())
		}
	}
	return result
}

// Copy returns a deep copy of the webhook configuration.
func (w *AdmissionWebhookConfig) Copy() *AdmissionWebhookConfig {
	if w == nil {
		return nil
	}
	nw := new(AdmissionWebhookConfig)
	*nw = *w
	nw.Namespaces = slices.Clone(w.Namespaces)
	nw.ExcludeNamespaces = slices.Clone(w.ExcludeNamespaces)
	nw.Resources = slices.Clone(w.Resources)
	nw.VariablePaths = slices.Clone(w.VariablePaths)
	nw.ExtraKeysHCL = slices.Clone(w.ExtraKeysHCL)
	return nw
}
