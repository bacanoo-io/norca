// Package client is a small HTTP client for uBixVault's system endpoints. It
// backs the `ubixvault operator` commands and can be reused by other tooling.
package client

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

// Client talks to a uBixVault server.
type Client struct {
	addr          string
	token         string
	hc            *http.Client
	tlsSkipVerify bool
	rootCAs       *x509.CertPool
}

// Option configures a [Client].
type Option func(*Client)

// WithTLSSkipVerify disables TLS certificate verification. INSECURE — intended
// for reaching a TLS server whose certificate does not cover the dial address
// (e.g. https://127.0.0.1 inside a pod, or a self-signed cert in testing).
func WithTLSSkipVerify(skip bool) Option {
	return func(c *Client) { c.tlsSkipVerify = skip }
}

// WithRootCAs sets the pool of CA certificates to trust for HTTPS, instead of
// the system roots. Use this to trust a private CA without skipping verification.
func WithRootCAs(pool *x509.CertPool) Option {
	return func(c *Client) { c.rootCAs = pool }
}

// New returns a client for the server at addr, sending token (may be empty) as
// the X-Vault-Token header. TLS behaviour is controlled by the options.
func New(addr, token string, opts ...Option) *Client {
	c := &Client{
		addr:  strings.TrimRight(addr, "/"),
		token: token,
	}
	for _, o := range opts {
		o(c)
	}
	hc := &http.Client{Timeout: 30 * time.Second}
	if c.tlsSkipVerify || c.rootCAs != nil {
		hc.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS12,
				InsecureSkipVerify: c.tlsSkipVerify, //nolint:gosec // G402: opt-in via -tls-skip-verify, documented as INSECURE
				RootCAs:            c.rootCAs,
			},
		}
	}
	c.hc = hc
	return c
}

// InitResult is returned by [Client.Init]. Keys are the Shamir unseal shares;
// in auto-unseal mode Keys is empty and RecoveryKeys holds the recovery shares.
type InitResult struct {
	Keys         []string `json:"keys"`
	KeysBase64   []string `json:"keys_base64"`
	RecoveryKeys []string `json:"recovery_keys"`
	RootToken    string   `json:"root_token"`
}

// SealStatus is the seal-status of the vault.
type SealStatus struct {
	Initialized bool `json:"initialized"`
	Sealed      bool `json:"sealed"`
	Threshold   int  `json:"t"`
	Shares      int  `json:"n"`
	Progress    int  `json:"progress"`
}

// Init initializes the vault with the given Shamir parameters.
func (c *Client) Init(ctx context.Context, shares, threshold int) (*InitResult, error) {
	body := map[string]int{"secret_shares": shares, "secret_threshold": threshold}
	var out InitResult
	if err := c.do(ctx, http.MethodPost, "/v1/sys/init", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Unseal submits a single unseal key and returns the resulting status.
func (c *Client) Unseal(ctx context.Context, key string) (*SealStatus, error) {
	var out SealStatus
	if err := c.do(ctx, http.MethodPost, "/v1/sys/unseal", map[string]string{"key": key}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SealStatus returns the current seal status.
func (c *Client) SealStatus(ctx context.Context) (*SealStatus, error) {
	var out SealStatus
	if err := c.do(ctx, http.MethodGet, "/v1/sys/seal-status", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Seal re-seals the vault. It requires a token.
func (c *Client) Seal(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/v1/sys/seal", nil, nil)
}

// RekeyStatus is the state of a rekey (unseal-share rotation) attempt. Keys is
// populated only on the update that completes the rekey.
type RekeyStatus struct {
	Started      bool     `json:"started"`
	Nonce        string   `json:"nonce"`
	Progress     int      `json:"progress"`
	Required     int      `json:"required"`
	NewShares    int      `json:"new_shares"`
	NewThreshold int      `json:"new_threshold"`
	Complete     bool     `json:"complete"`
	Keys         []string `json:"keys"`
	KeysBase64   []string `json:"keys_base64"`
}

// RekeyInit starts a rekey that will re-split the master key into shares/threshold
// new unseal shares.
func (c *Client) RekeyInit(ctx context.Context, shares, threshold int) (*RekeyStatus, error) {
	body := map[string]int{"secret_shares": shares, "secret_threshold": threshold}
	var out RekeyStatus
	if err := c.do(ctx, http.MethodPost, "/v1/sys/rekey/init", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RekeyUpdate submits one current unseal key to the attempt identified by nonce.
// The returned status carries the new shares once Complete is true.
func (c *Client) RekeyUpdate(ctx context.Context, nonce, key string) (*RekeyStatus, error) {
	body := map[string]string{"nonce": nonce, "key": key}
	var out RekeyStatus
	if err := c.do(ctx, http.MethodPost, "/v1/sys/rekey/update", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RekeyStatus returns the current rekey attempt, if any.
func (c *Client) RekeyStatus(ctx context.Context) (*RekeyStatus, error) {
	var out RekeyStatus
	if err := c.do(ctx, http.MethodGet, "/v1/sys/rekey/init", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RekeyCancel aborts any in-progress rekey attempt.
func (c *Client) RekeyCancel(ctx context.Context) error {
	return c.do(ctx, http.MethodDelete, "/v1/sys/rekey/init", nil, nil)
}

// Snapshot streams a backup of the encrypted store to w. It requires a token.
func (c *Client) Snapshot(ctx context.Context, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.addr+"/v1/sys/snapshot", nil)
	if err != nil {
		return fmt.Errorf("client: new request: %w", err)
	}
	if c.token != "" {
		req.Header.Set("X-Vault-Token", c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("client: snapshot: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		return &APIError{StatusCode: resp.StatusCode, Errors: parseErrors(data)}
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		return fmt.Errorf("client: read snapshot: %w", err)
	}
	return nil
}

// do performs a request, encoding body as JSON and decoding a successful
// response into out (if non-nil). Non-2xx responses are turned into errors.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("client: marshal request: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.addr+path, reqBody)
	if err != nil {
		return fmt.Errorf("client: new request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("X-Vault-Token", c.token)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("client: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("client: read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{StatusCode: resp.StatusCode, Errors: parseErrors(data)}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("client: decode response: %w", err)
		}
	}
	return nil
}

// APIError is returned for non-2xx responses.
type APIError struct {
	StatusCode int
	Errors     []string
}

func (e *APIError) Error() string {
	if len(e.Errors) > 0 {
		return fmt.Sprintf("server returned %d: %s", e.StatusCode, strings.Join(e.Errors, "; "))
	}
	return fmt.Sprintf("server returned %d", e.StatusCode)
}

func parseErrors(data []byte) []string {
	var e struct {
		Errors []string `json:"errors"`
	}
	if json.Unmarshal(data, &e) == nil {
		return e.Errors
	}
	return nil
}
