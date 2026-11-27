// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"strings"
	"sync/atomic"

	"google.golang.org/grpc/credentials"
)

// maxHeadersFileSize bounds the headers file; it holds a few credentials.
const maxHeadersFileSize = 64 << 10

// normalizeHeaders checks the header names and values and returns them
// with the names in lower case, as gRPC metadata requires. Errors name the
// header, never its value.
func normalizeHeaders(in map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(in))
	var errs []error
	for k, val := range in {
		name := strings.ToLower(strings.TrimSpace(k))
		if err := checkHeader(name, val); err != nil {
			errs = append(errs, err)
			continue
		}
		if _, dup := out[name]; dup {
			errs = append(errs, fmt.Errorf("header %q is given twice", name))
			continue
		}
		out[name] = val
	}
	return out, errors.Join(errs...)
}

// checkHeader checks a lower-case header name and its value against the
// gRPC metadata rules.
func checkHeader(name, value string) error {
	switch {
	case name == "":
		return errors.New("a header name is empty")
	case strings.HasPrefix(name, "grpc-") || strings.HasPrefix(name, ":"):
		return fmt.Errorf("header %q is reserved by gRPC", name)
	case strings.HasSuffix(name, "-bin"):
		return fmt.Errorf("header %q: binary headers are not supported", name)
	}
	for _, r := range name {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.'
		if !ok {
			return fmt.Errorf("header %q has a character that gRPC metadata does not allow", name)
		}
	}
	for i := range len(value) {
		if c := value[i]; c < 0x20 || c > 0x7e {
			return fmt.Errorf("the value of header %q has a character that gRPC metadata does not allow", name)
		}
	}
	return nil
}

// readHeadersFile reads a headers file: one key=value per line; blank lines
// and lines starting with # are skipped, and spaces around the key and the
// value are trimmed. Errors name the line, never its content.
func readHeadersFile(path string) (map[string]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("headers_file: %w", err)
	}
	if info.Size() > maxHeadersFileSize {
		return nil, fmt.Errorf("headers_file %s is larger than %d bytes", path, maxHeadersFileSize)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("headers_file: %w", err)
	}
	headers := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		key, value, ok := strings.Cut(text, "=")
		if !ok {
			return nil, fmt.Errorf("headers_file %s line %d: not key=value", path, line)
		}
		name := strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		if err := checkHeader(name, value); err != nil {
			return nil, fmt.Errorf("headers_file %s line %d: %w", path, line, err)
		}
		if _, dup := headers[name]; dup {
			return nil, fmt.Errorf("headers_file %s line %d: header %q is given twice", path, line, name)
		}
		headers[name] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("headers_file %s: %w", path, err)
	}
	return headers, nil
}

// headerCredentials sends the headers of one signal with every export as
// gRPC metadata: the inline headers and those of the headers file, which
// win. The file is read again by reload, so a SIGHUP takes a rotated
// credential without reconnecting.
type headerCredentials struct {
	inline map[string]string
	file   string

	// requireTLS refuses to send over a plaintext connection; it is false
	// only for a plaintext target, which Resolve allows on loopback only.
	requireTLS bool

	current atomic.Pointer[map[string]string]
}

var _ credentials.PerRPCCredentials = (*headerCredentials)(nil)

// newHeaderCredentials reads the headers file of the target, if any.
func newHeaderCredentials(t Target) (*headerCredentials, error) {
	h := &headerCredentials{
		inline:     maps.Clone(t.Headers),
		file:       t.HeadersFile,
		requireTLS: !t.Plaintext,
	}
	if err := h.reload(); err != nil {
		return nil, err
	}
	return h, nil
}

// reload reads the headers file again. On an error the headers in use stay.
func (h *headerCredentials) reload() error {
	merged := maps.Clone(h.inline)
	if merged == nil {
		merged = map[string]string{}
	}
	if h.file != "" {
		fromFile, err := readHeadersFile(h.file)
		if err != nil {
			return err
		}
		maps.Copy(merged, fromFile)
	}
	h.current.Store(&merged)
	return nil
}

// GetRequestMetadata implements credentials.PerRPCCredentials. It returns a
// copy, one per export, so the caller may keep or change it.
func (h *headerCredentials) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return maps.Clone(*h.current.Load()), nil
}

// RequireTransportSecurity implements credentials.PerRPCCredentials.
func (h *headerCredentials) RequireTransportSecurity() bool {
	return h.requireTLS
}
