package kubeauth

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// saUsernamePrefix is how the Kubernetes API names a ServiceAccount identity:
// "system:serviceaccount:<namespace>:<name>".
const saUsernamePrefix = "system:serviceaccount:"

// httpReviewer validates tokens by calling the Kubernetes TokenReview API.
type httpReviewer struct {
	host        string
	reviewerJWT string
	hc          *http.Client
}

// newHTTPReviewer builds a reviewer from config, trusting the supplied CA (if any).
func newHTTPReviewer(cfg Config) (TokenReviewer, error) {
	transport := &http.Transport{}
	if cfg.CACert != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(cfg.CACert)) {
			return nil, fmt.Errorf("kubeauth: invalid CA certificate")
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &httpReviewer{
		host:        strings.TrimRight(cfg.Host, "/"),
		reviewerJWT: cfg.ReviewerJWT,
		hc:          &http.Client{Timeout: 30 * time.Second, Transport: transport},
	}, nil
}

// tokenReviewRequest / tokenReviewResponse mirror the authentication.k8s.io/v1
// TokenReview object (only the fields we use).
type tokenReviewRequest struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Spec       tokenReviewSpec `json:"spec"`
}

type tokenReviewSpec struct {
	Token string `json:"token"`
}

type tokenReviewResponse struct {
	Status struct {
		Authenticated bool `json:"authenticated"`
		User          struct {
			Username string `json:"username"`
		} `json:"user"`
		Error string `json:"error"`
	} `json:"status"`
}

// Review posts a TokenReview to the apiserver and interprets the result.
func (r *httpReviewer) Review(ctx context.Context, jwt string) (*ReviewResult, error) {
	body, err := json.Marshal(tokenReviewRequest{
		APIVersion: "authentication.k8s.io/v1",
		Kind:       "TokenReview",
		Spec:       tokenReviewSpec{Token: jwt},
	})
	if err != nil {
		return nil, fmt.Errorf("kubeauth: marshal review: %w", err)
	}

	url := r.host + "/apis/authentication.k8s.io/v1/tokenreviews"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("kubeauth: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if r.reviewerJWT != "" {
		req.Header.Set("Authorization", "Bearer "+r.reviewerJWT)
	}

	resp, err := r.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kubeauth: token review: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("kubeauth: read review response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("kubeauth: token review returned %d", resp.StatusCode)
	}

	var out tokenReviewResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("kubeauth: decode review response: %w", err)
	}
	if !out.Status.Authenticated {
		return &ReviewResult{Authenticated: false}, nil
	}

	namespace, sa, ok := parseServiceAccount(out.Status.User.Username)
	if !ok {
		// Authenticated, but not a ServiceAccount identity we can bind.
		return &ReviewResult{Authenticated: false}, nil
	}
	return &ReviewResult{Authenticated: true, Namespace: namespace, ServiceAccount: sa}, nil
}

// parseServiceAccount extracts the namespace and name from a ServiceAccount
// username of the form "system:serviceaccount:<namespace>:<name>".
func parseServiceAccount(username string) (namespace, name string, ok bool) {
	rest, found := strings.CutPrefix(username, saUsernamePrefix)
	if !found {
		return "", "", false
	}
	ns, sa, found := strings.Cut(rest, ":")
	if !found || ns == "" || sa == "" {
		return "", "", false
	}
	return ns, sa, true
}
