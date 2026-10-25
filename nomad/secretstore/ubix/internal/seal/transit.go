package seal

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Transit wraps the master key via a remote Vault-compatible Transit engine
// (uBixVault or HashiCorp Vault). The wrapping key lives in that vault and
// never reaches this host; this host only holds a token authorized to encrypt
// and decrypt with the named key.
type Transit struct {
	addr  string // base URL, no trailing slash
	token string
	key   string
	hc    *http.Client
}

// TransitConfig configures a [Transit] seal.
type TransitConfig struct {
	Address    string // e.g. https://seal-vault:8200
	Token      string // token with encrypt/decrypt on Key
	Key        string // transit key name
	CACertPEM  []byte // optional CA bundle to trust
	SkipVerify bool   // skip TLS verification (INSECURE)
}

// NewTransit builds a transit seal from cfg.
func NewTransit(cfg TransitConfig) (*Transit, error) {
	if cfg.Address == "" || cfg.Token == "" || cfg.Key == "" {
		return nil, fmt.Errorf("seal: transit requires address, token, and key")
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.SkipVerify} //nolint:gosec // G402: opt-in via config
	if len(cfg.CACertPEM) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(cfg.CACertPEM) {
			return nil, fmt.Errorf("seal: transit ca-cert: no certificates found")
		}
		tlsCfg.RootCAs = pool
	}
	return &Transit{
		addr:  strings.TrimRight(cfg.Address, "/"),
		token: cfg.Token,
		key:   cfg.Key,
		hc:    &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg}},
	}, nil
}

// Type implements [Seal].
func (t *Transit) Type() string { return "transit" }

// Wrap encrypts plaintext via transit/encrypt and returns the ciphertext string.
func (t *Transit) Wrap(ctx context.Context, plaintext []byte) ([]byte, error) {
	var out struct {
		Data struct {
			Ciphertext string `json:"ciphertext"`
		} `json:"data"`
	}
	body := map[string]string{"plaintext": base64.StdEncoding.EncodeToString(plaintext)}
	if err := t.do(ctx, "/v1/transit/encrypt/"+t.key, body, &out); err != nil {
		return nil, err
	}
	if out.Data.Ciphertext == "" {
		return nil, fmt.Errorf("seal: transit encrypt returned no ciphertext")
	}
	return []byte(out.Data.Ciphertext), nil
}

// Unwrap decrypts the stored ciphertext via transit/decrypt.
func (t *Transit) Unwrap(ctx context.Context, wrapped []byte) ([]byte, error) {
	var out struct {
		Data struct {
			Plaintext string `json:"plaintext"`
		} `json:"data"`
	}
	body := map[string]string{"ciphertext": string(wrapped)}
	if err := t.do(ctx, "/v1/transit/decrypt/"+t.key, body, &out); err != nil {
		return nil, err
	}
	plaintext, err := base64.StdEncoding.DecodeString(out.Data.Plaintext)
	if err != nil {
		return nil, fmt.Errorf("seal: transit decrypt returned invalid plaintext: %w", err)
	}
	return plaintext, nil
}

func (t *Transit) do(ctx context.Context, path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("seal: transit marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.addr+path, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("seal: transit request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Vault-Token", t.token)

	resp, err := t.hc.Do(req)
	if err != nil {
		return fmt.Errorf("seal: transit %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("seal: transit read: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("seal: transit %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("seal: transit decode: %w", err)
	}
	return nil
}
