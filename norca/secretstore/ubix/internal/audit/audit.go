// Package audit records a tamper-aware log of who accessed what, when, and
// whether it succeeded (docs/DESIGN.md §3.7).
//
// An [Entry] is fanned out by a [Broker] to one or more [Device]s. Auditing is
// fail-closed: if a device cannot record a request entry, the broker returns an
// error and the caller refuses the request, so nothing proceeds unaudited.
//
// Devices never write the client token in the clear. Sensitive fields are
// HMAC'd with a key the vault keeps in its barrier (see [Broker.SetHMACKey]), so
// the log can correlate activity by token — across restarts and replicas —
// without exposing the credential itself. While no key is set (the vault is
// sealed) the token is omitted entirely.
package audit

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// Entry is a single audited event. The engine populates it; the device decides
// how to render it (in particular, it HMACs ClientToken rather than writing it).
type Entry struct {
	Time        time.Time
	Type        string // "request" or "response"
	Operation   string // read / create / update / delete / list
	Path        string
	ClientToken string // raw token; HMAC'd by the device, never written in the clear
	RemoteAddr  string
	StatusCode  int // response entries only
}

// Device records audit entries somewhere durable.
type Device interface {
	Log(ctx context.Context, e *Entry) error
	Close() error
}

// KeyedDevice is a [Device] that HMACs tokens with a key supplied by the vault.
type KeyedDevice interface {
	Device
	// SetHMACKey installs the token HMAC key; nil clears it.
	SetHMACKey(key []byte)
}

// logLine is the on-disk JSON shape. Note the absence of a raw-token field.
type logLine struct {
	Time       string `json:"time"`
	Type       string `json:"type"`
	Operation  string `json:"operation,omitempty"`
	Path       string `json:"path,omitempty"`
	TokenHMAC  string `json:"token_hmac,omitempty"`
	RemoteAddr string `json:"remote_addr,omitempty"`
	StatusCode int    `json:"status_code,omitempty"`
}

// FileDevice appends newline-delimited JSON entries to a file.
type FileDevice struct {
	mu      sync.Mutex
	f       *os.File
	hmacKey []byte // nil until SetHMACKey; tokens are omitted meanwhile
}

var _ KeyedDevice = (*FileDevice)(nil)

// NewFileDevice opens (or creates) path for appending. Tokens are omitted from
// entries until [FileDevice.SetHMACKey] supplies the vault's HMAC key.
func NewFileDevice(path string) (*FileDevice, error) {
	// path is an operator-provided configuration value (a server flag), not
	// attacker-controlled input.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: operator-configured audit log path
	if err != nil {
		return nil, fmt.Errorf("audit: open %q: %w", path, err)
	}
	return &FileDevice{f: f}, nil
}

// SetHMACKey installs the key used to HMAC tokens; nil clears it.
func (d *FileDevice) SetHMACKey(key []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if key == nil {
		d.hmacKey = nil
		return
	}
	d.hmacKey = append([]byte(nil), key...)
}

// Log writes one entry as a JSON line, HMACing the client token.
func (d *FileDevice) Log(_ context.Context, e *Entry) error {
	line := logLine{
		Time:       e.Time.UTC().Format(time.RFC3339Nano),
		Type:       e.Type,
		Operation:  e.Operation,
		Path:       e.Path,
		RemoteAddr: e.RemoteAddr,
		StatusCode: e.StatusCode,
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if e.ClientToken != "" && d.hmacKey != nil {
		line.TokenHMAC = hmacToken(d.hmacKey, e.ClientToken)
	}
	data, err := json.Marshal(line)
	if err != nil {
		return fmt.Errorf("audit: marshal: %w", err)
	}
	data = append(data, '\n')
	if _, err := d.f.Write(data); err != nil {
		return fmt.Errorf("audit: write: %w", err)
	}
	return nil
}

// Close closes the underlying file.
func (d *FileDevice) Close() error { return d.f.Close() }

func hmacToken(key []byte, token string) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(token))
	return hex.EncodeToString(mac.Sum(nil))
}

// Broker fans an entry out to every configured device. It is safe for concurrent
// use (devices must be too).
type Broker struct {
	devices []Device
	now     func() time.Time
}

// NewBroker returns a broker over the given devices.
func NewBroker(devices ...Device) *Broker {
	return &Broker{devices: devices, now: func() time.Time { return time.Now().UTC() }}
}

// SetHMACKey hands key to every [KeyedDevice]; nil clears it. The vault calls it
// once unsealed, with the key it keeps in the barrier, and with nil on seal.
func (b *Broker) SetHMACKey(key []byte) {
	for _, d := range b.devices {
		if kd, ok := d.(KeyedDevice); ok {
			kd.SetHMACKey(key)
		}
	}
}

// LogRequest records a request entry. It is fail-closed: if any device errors,
// the error is returned and the request must be refused.
func (b *Broker) LogRequest(ctx context.Context, e *Entry) error {
	e.Type = "request"
	return b.log(ctx, e)
}

// LogResponse records a response entry.
func (b *Broker) LogResponse(ctx context.Context, e *Entry) error {
	e.Type = "response"
	return b.log(ctx, e)
}

func (b *Broker) log(ctx context.Context, e *Entry) error {
	if e.Time.IsZero() {
		e.Time = b.now()
	}
	for _, d := range b.devices {
		if err := d.Log(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

// Close closes every device, returning the first error.
func (b *Broker) Close() error {
	var firstErr error
	for _, d := range b.devices {
		if err := d.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
