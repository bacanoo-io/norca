// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package admission

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	metrics "github.com/armon/go-metrics"
	"github.com/hashicorp/go-cleanhttp"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/nomad/structs/config"
)

const (
	// ReviewAPIVersion and ReviewKind identify the admission review
	// documents exchanged with webhooks.
	ReviewAPIVersion = "admission.norca.io/v1"
	ReviewKind       = "AdmissionReview"

	// maxResponseSize limits the size of a webhook response body.
	maxResponseSize = 16 << 20
)

// Review is the document POSTed to a webhook (with Request set) and expected
// back from it (with Response set). Its structure follows the Kubernetes
// AdmissionReview.
type Review struct {
	APIVersion string          `json:"api_version"`
	Kind       string          `json:"kind"`
	Request    *ReviewRequest  `json:"request,omitempty"`
	Response   *ReviewResponse `json:"response,omitempty"`
}

// ReviewRequest describes the job submission under review.
type ReviewRequest struct {
	// UID identifies the review; the response must echo it.
	UID string `json:"uid"`

	// Operation is "register" or "plan".
	Operation string `json:"operation"`

	// DryRun is true for plan.
	DryRun bool `json:"dry_run"`

	// Namespace is the namespace of the job.
	Namespace string `json:"namespace"`

	// User is the submitter.
	User *ReviewUser `json:"user"`

	// Job is the complete job in the Nomad API JSON form, without secrets.
	Job json.RawMessage `json:"job"`
}

// ReviewUser identifies the submitter of the job.
type ReviewUser struct {
	AccessorID string   `json:"accessor_id,omitempty"`
	Name       string   `json:"name,omitempty"`
	Type       string   `json:"type,omitempty"`
	Roles      []string `json:"roles,omitempty"`
	Policies   []string `json:"policies,omitempty"`
}

// NewReviewUser converts the submitter into its wire form.
func NewReviewUser(u *structs.AdmissionUser) *ReviewUser {
	if u == nil {
		return nil
	}
	return &ReviewUser{
		AccessorID: u.AccessorID,
		Name:       u.Name,
		Type:       u.Type,
		Roles:      u.Roles,
		Policies:   u.Policies,
	}
}

// ReviewResponse is the decision of a webhook.
type ReviewResponse struct {
	UID      string   `json:"uid"`
	Allowed  bool     `json:"allowed"`
	Message  string   `json:"message,omitempty"`
	Warnings []string `json:"warnings,omitempty"`

	// Patch is an optional RFC 6902 JSON Patch against the job, honored for
	// mutating webhooks only. It may be given as a JSON array or as a
	// base64 encoded string of one.
	Patch json.RawMessage `json:"patch,omitempty"`
}

// PatchOperations returns the JSON patch of the response, or nil if there is
// none.
func (r *ReviewResponse) PatchOperations() ([]byte, error) {
	p := bytes.TrimSpace(r.Patch)
	if len(p) == 0 || bytes.Equal(p, []byte("null")) {
		return nil, nil
	}
	if p[0] == '"' {
		var s string
		if err := json.Unmarshal(p, &s); err != nil {
			return nil, fmt.Errorf("invalid patch: %w", err)
		}
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, fmt.Errorf("invalid base64 patch: %w", err)
		}
		p = bytes.TrimSpace(b)
	}
	if len(p) == 0 || p[0] != '[' {
		return nil, errors.New("patch must be a JSON array")
	}
	if bytes.Equal(p, []byte("[]")) {
		return nil, nil
	}
	return p, nil
}

// Webhook is a configured admission webhook with its own pooled HTTP client.
type Webhook struct {
	cfg    *config.AdmissionWebhookConfig
	client *http.Client
}

// NewWebhook builds a webhook client from its configuration. The
// configuration must be canonicalized and validated.
func NewWebhook(cfg *config.AdmissionWebhookConfig) (*Webhook, error) {
	transport := cleanhttp.DefaultPooledTransport()

	tlsConf := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: cfg.TLSServerName,
	}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("admission webhook %q: failed to read ca_file: %w", cfg.Name, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("admission webhook %q: no certificates found in ca_file", cfg.Name)
		}
		tlsConf.RootCAs = pool
	}
	if cfg.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("admission webhook %q: failed to load client certificate: %w", cfg.Name, err)
		}
		tlsConf.Certificates = []tls.Certificate{cert}
	}
	transport.TLSClientConfig = tlsConf

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = config.DefaultAdmissionWebhookTimeout
	}

	return &Webhook{
		cfg: cfg,
		client: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			// Never follow redirects: the job must only be sent to the
			// configured URL.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// Name returns the name of the webhook.
func (w *Webhook) Name() string { return w.cfg.Name }

// Mutating returns true if the webhook may modify jobs.
func (w *Webhook) Mutating() bool { return w.cfg.Mode == config.AdmissionWebhookModeMutate }

// IgnoreFailure returns true if a failed call admits the job.
func (w *Webhook) IgnoreFailure() bool {
	return w.cfg.FailurePolicy == config.AdmissionFailurePolicyIgnore
}

// AppliesTo returns true if jobs in the namespace are sent to the webhook.
func (w *Webhook) AppliesTo(namespace string) bool {
	if structs.AdmissionNamespaceMatches(w.cfg.ExcludeNamespaces, namespace, false) {
		return false
	}
	return structs.AdmissionNamespaceMatches(w.cfg.Namespaces, namespace, true)
}

// Close releases the idle connections of the webhook.
func (w *Webhook) Close() {
	w.client.CloseIdleConnections()
}

// Review sends the review request to the webhook once, without retries, and
// returns its validated response. Any error is a call failure that is
// subject to the webhook's failure policy.
func (w *Webhook) Review(ctx context.Context, req *ReviewRequest) (resp *ReviewResponse, err error) {
	labels := []metrics.Label{{Name: "webhook", Value: w.cfg.Name}}
	defer metrics.MeasureSinceWithLabels([]string{"nomad", "admission", "webhook", "latency"}, time.Now(), labels)
	metrics.IncrCounterWithLabels([]string{"nomad", "admission", "webhook", "calls"}, 1, labels)
	defer func() {
		if err != nil {
			metrics.IncrCounterWithLabels([]string{"nomad", "admission", "webhook", "errors"}, 1, labels)
		}
	}()

	body, err := json.Marshal(&Review{
		APIVersion: ReviewAPIVersion,
		Kind:       ReviewKind,
		Request:    req,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to encode review: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, w.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	httpResp, err := w.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(httpResp.Body, maxResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}
	if len(respBody) > maxResponseSize {
		return nil, fmt.Errorf("response larger than %d bytes", maxResponseSize)
	}
	if httpResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected response status %d", httpResp.StatusCode)
	}

	var review Review
	if err := json.Unmarshal(respBody, &review); err != nil {
		return nil, fmt.Errorf("invalid response: %w", err)
	}
	if review.Response == nil {
		return nil, errors.New("invalid response: missing response object")
	}
	if review.Response.UID != req.UID {
		return nil, fmt.Errorf("invalid response: uid %q does not match request uid %q", review.Response.UID, req.UID)
	}
	return review.Response, nil
}
