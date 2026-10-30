// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package consulcompat

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/hashicorp/nomad/acl"
	structsc "github.com/hashicorp/nomad/nomad/structs/config"
)

// maxKVValueSize is the largest value Consul accepts.
const maxKVValueSize = 512 * 1024

// kvEntry is a key of the Consul KV.
type kvEntry struct {
	Key         string
	Value       []byte
	Flags       uint64
	CreateIndex uint64
	ModifyIndex uint64
}

// kvPair is the JSON form of a key in the Consul API.
type kvPair struct {
	Key         string
	CreateIndex uint64
	ModifyIndex uint64
	LockIndex   uint64
	Flags       uint64
	Value       []byte
}

// kvStore is a backend of the Consul KV.
type kvStore interface {
	// Get returns the key, or the keys with the prefix key, and the index
	// of the result. With r.index set it blocks until the index exceeds
	// r.index or r.wait passes.
	Get(r *request, key string, prefix bool) ([]*kvEntry, uint64, error)

	// Put stores e. With cas set it only stores when the ModifyIndex of the
	// key is *cas (0: the key must not exist) and reports false otherwise.
	Put(r *request, e *kvEntry, cas *uint64) (bool, error)

	// Delete deletes the key, or the keys with the prefix key. With cas set
	// it only deletes when the ModifyIndex of the key is *cas.
	Delete(r *request, key string, recurse bool, cas *uint64) (bool, error)

	// Close releases the resources of the backend.
	Close()
}

// kvACL is how a backend that does not enforce the Nomad ACLs itself has
// the Variables rules of the keys applied.
type kvACL struct {
	backend   Backend
	namespace string
	path      string
}

// allow returns an error unless the token of r has the Variables
// capability op on the path of key.
func (a *kvACL) allow(r *request, op, key string) error {
	obj, err := a.backend.ResolveACL(r.token)
	if err != nil {
		return err
	}
	if !a.allowed(obj, op, key) {
		return &codedError{http.StatusForbidden, "Permission denied"}
	}
	return nil
}

// allowed reports whether obj (nil: ACLs disabled) has op on key.
func (a *kvACL) allowed(obj *acl.ACL, op, key string) bool {
	return obj == nil || obj.AllowVariableOperation(a.namespace, kvACLPath(a.path, key), op, nil)
}

// newKVStore returns the store of the kv block, nil when it is disabled.
func newKVStore(s *Server, cfg *structsc.ConsulCompatKVConfig) kvStore {
	namespace := s.cfg.Namespaces[0]
	if cfg != nil && cfg.Namespace != "" {
		namespace = cfg.Namespace
	}
	switch cfg.BackendName() {
	case structsc.ConsulCompatKVNATS:
		return newNATSKV(s.logger, cfg.NATS, &kvACL{backend: s.backend, namespace: namespace, path: cfg.PathPrefix()})
	case structsc.ConsulCompatKVDisabled:
		return nil
	default:
		return &variablesKV{backend: s.backend, namespace: namespace, path: cfg.PathPrefix()}
	}
}

// serveKV serves /v1/kv/<key>.
func (s *Server) serveKV(w http.ResponseWriter, req *http.Request, key string) {
	q := req.URL.Query()
	for _, p := range []string{"acquire", "release"} {
		if _, ok := q[p]; ok {
			http.Error(w, fmt.Sprintf("Not Implemented: ?%s is not supported by the Consul-compatible API of Norca; "+
				"it has no sessions", p), http.StatusNotImplemented)
			return
		}
	}

	r, err := s.parseRequest(req)
	if err != nil {
		s.writeError(w, err)
		return
	}

	switch req.Method {
	case http.MethodGet, http.MethodHead:
		err = s.kvGet(w, r, key)
	case http.MethodPut:
		err = s.kvPut(w, r, key)
	case http.MethodDelete:
		err = s.kvDelete(w, r, key)
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		err = &codedError{http.StatusMethodNotAllowed, fmt.Sprintf("method %s not allowed", req.Method)}
	}
	if err != nil {
		s.writeError(w, err)
	}
}

func (s *Server) kvGet(w http.ResponseWriter, r *request, key string) error {
	q := r.http.URL.Query()
	_, recurse := q["recurse"]
	_, keys := q["keys"]
	_, raw := q["raw"]
	if key == "" && !recurse && !keys {
		return badRequest("Missing key name")
	}

	entries, index, err := s.kv.Get(r, key, recurse || keys)
	if err != nil {
		return err
	}
	w.Header().Set("X-Consul-Index", strconv.FormatUint(max(index, 1), 10))
	w.Header().Set("X-Consul-KnownLeader", "true")
	w.Header().Set("X-Consul-LastContact", "0")
	if len(entries) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })

	var out any
	switch {
	case keys:
		out = kvKeys(entries, key, q.Get("separator"))
	case raw && !recurse:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, err := w.Write(entries[0].Value)
		return err
	default:
		pairs := make([]*kvPair, len(entries))
		for i, e := range entries {
			pairs[i] = &kvPair{Key: e.Key, CreateIndex: e.CreateIndex, ModifyIndex: e.ModifyIndex, Flags: e.Flags, Value: e.Value}
		}
		out = pairs
	}
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(out)
}

// kvKeys returns the names of entries; with a separator the keys below the
// prefix are cut after the first separator and listed once.
func kvKeys(entries []*kvEntry, prefix, separator string) []string {
	var keys []string
	seen := make(map[string]bool)
	for _, e := range entries {
		k := e.Key
		if separator != "" {
			if i := strings.Index(k[len(prefix):], separator); i >= 0 {
				k = k[:len(prefix)+i+len(separator)]
			}
		}
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	return keys
}

func (s *Server) kvPut(w http.ResponseWriter, r *request, key string) error {
	if key == "" {
		return badRequest("Missing key name")
	}
	q := r.http.URL.Query()
	e := &kvEntry{Key: key}
	if raw := q.Get("flags"); raw != "" {
		flags, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return badRequest("Invalid flags: %v", err)
		}
		e.Flags = flags
	}
	cas, err := parseCAS(q.Get("cas"))
	if err != nil {
		return err
	}

	value, err := io.ReadAll(io.LimitReader(r.http.Body, maxKVValueSize+1))
	if err != nil {
		return err
	}
	if len(value) > maxKVValueSize {
		return &codedError{http.StatusRequestEntityTooLarge,
			fmt.Sprintf("Value exceeds %d byte limit", maxKVValueSize)}
	}
	e.Value = value

	ok, err := s.kv.Put(r, e, cas)
	if err != nil {
		return err
	}
	return writeBool(w, ok)
}

func (s *Server) kvDelete(w http.ResponseWriter, r *request, key string) error {
	q := r.http.URL.Query()
	_, recurse := q["recurse"]
	if key == "" && !recurse {
		return badRequest("Missing key name")
	}
	cas, err := parseCAS(q.Get("cas"))
	if err != nil {
		return err
	}
	if cas != nil && recurse {
		return badRequest("Conflicting flags: cas, recurse")
	}
	ok, err := s.kv.Delete(r, key, recurse, cas)
	if err != nil {
		return err
	}
	return writeBool(w, ok)
}

func parseCAS(raw string) (*uint64, error) {
	if raw == "" {
		return nil, nil
	}
	cas, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return nil, badRequest("Invalid cas index: %v", err)
	}
	return &cas, nil
}

func writeBool(w http.ResponseWriter, ok bool) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(ok)
}

// The keys of the Consul KV are mapped to Variables paths below the path of
// the kv block. A Variables path allows letters, digits, "-", "_", "~" and
// "/" only, so every other byte of a key, "~" included, becomes "~XX" (its
// hexadecimal value) and an empty segment ("a//b", "a/") becomes "~". The
// mapping keeps prefixes: the keys with a prefix are the Variables with the
// mapped prefix.

// encodeKVKey maps a key to a path relative to the path of the kv block.
// With prefix set an empty last segment stays empty, so the result is a
// prefix of the paths of the keys below it.
func encodeKVKey(key string, prefix bool) string {
	segments := strings.Split(key, "/")
	for i, seg := range segments {
		if seg == "" && !(prefix && i == len(segments)-1) {
			segments[i] = "~"
			continue
		}
		var b strings.Builder
		for j := 0; j < len(seg); j++ {
			c := seg[j]
			if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
				b.WriteByte(c)
			} else {
				fmt.Fprintf(&b, "~%02X", c)
			}
		}
		segments[i] = b.String()
	}
	return strings.Join(segments, "/")
}

// decodeKVKey reverses encodeKVKey.
func decodeKVKey(path string) (string, error) {
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		if seg == "~" {
			segments[i] = ""
			continue
		}
		var b strings.Builder
		for j := 0; j < len(seg); j++ {
			if seg[j] != '~' {
				b.WriteByte(seg[j])
				continue
			}
			if j+2 >= len(seg) {
				return "", fmt.Errorf("invalid escape in %q", path)
			}
			v, err := strconv.ParseUint(seg[j+1:j+3], 16, 8)
			if err != nil {
				return "", fmt.Errorf("invalid escape in %q", path)
			}
			b.WriteByte(byte(v))
			j += 2
		}
		segments[i] = b.String()
	}
	return strings.Join(segments, "/"), nil
}

// kvACLPath returns the Variables path whose ACL rules apply to key.
func kvACLPath(base, key string) string {
	return base + "/" + encodeKVKey(key, false)
}
