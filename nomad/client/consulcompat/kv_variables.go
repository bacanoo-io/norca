// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package consulcompat

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/hashicorp/nomad/nomad/structs"
)

// maxVariablePath is the length of a Variables path.
const maxVariablePath = 128

// Items of the Variable of a key.
const (
	kvItemValue       = "value"        // a UTF-8 value
	kvItemValueBase64 = "value_base64" // any other value, base64
	kvItemFlags       = "flags"        // the flags, when not 0
)

// variablesKV keeps the Consul KV in Norca Variables: the key "app/config"
// is the Variable "<path>/app/config" of the namespace. The servers apply
// the Variables ACL rules of the request token; writes go through Raft and
// the Variables are encrypted and backed up like all others.
type variablesKV struct {
	backend   Backend
	namespace string
	path      string
}

func (v *variablesKV) varPath(key string) (string, error) {
	p := v.path + "/" + encodeKVKey(key, false)
	if len(p) > maxVariablePath {
		return "", &codedError{http.StatusBadRequest, fmt.Sprintf(
			"key %q is too long for the variables backend of the KV: its Variables path %q has %d characters, at most %d are allowed",
			key, p, len(p), maxVariablePath)}
	}
	return p, nil
}

func (v *variablesKV) queryOptions(r *request, blocking bool) structs.QueryOptions {
	opts := r.queryOptions(v.namespace, blocking)
	return opts
}

func (v *variablesKV) Get(r *request, key string, prefix bool) ([]*kvEntry, uint64, error) {
	if !prefix {
		path, err := v.varPath(key)
		if err != nil {
			return nil, 0, err
		}
		var resp structs.VariablesReadResponse
		args := &structs.VariablesReadRequest{Path: path, QueryOptions: v.queryOptions(r, true)}
		if err := v.backend.RPC(structs.VariablesReadRPCMethod, args, &resp); err != nil {
			return nil, 0, err
		}
		if resp.Data == nil {
			return nil, resp.Index, nil
		}
		e, err := v.entry(resp.Data)
		if err != nil {
			return nil, 0, err
		}
		return []*kvEntry{e}, resp.Index, nil
	}

	// The list blocks; the values are read after it.
	opts := v.queryOptions(r, true)
	opts.Prefix = v.path + "/" + encodeKVKey(key, true)
	var metas []*structs.VariableMetadata
	var index uint64
	for {
		var resp structs.VariablesListResponse
		if err := v.backend.RPC(structs.VariablesListRPCMethod, &structs.VariablesListRequest{QueryOptions: opts}, &resp); err != nil {
			return nil, 0, err
		}
		metas = append(metas, resp.Data...)
		index = max(index, resp.Index)
		if resp.NextToken == "" {
			break
		}
		opts.NextToken = resp.NextToken
		opts.MinQueryIndex = 0
	}

	var entries []*kvEntry
	for _, m := range metas {
		var resp structs.VariablesReadResponse
		args := &structs.VariablesReadRequest{Path: m.Path, QueryOptions: v.queryOptions(r, false)}
		if err := v.backend.RPC(structs.VariablesReadRPCMethod, args, &resp); err != nil {
			if strings.Contains(err.Error(), structs.ErrPermissionDenied.Error()) {
				continue // listed, but not readable
			}
			return nil, 0, err
		}
		if resp.Data == nil {
			continue // deleted since the list
		}
		e, err := v.entry(resp.Data)
		if err != nil {
			continue // not a key of the KV
		}
		if strings.HasPrefix(e.Key, key) {
			entries = append(entries, e)
		}
	}
	return entries, index, nil
}

// entry converts the Variable of a key.
func (v *variablesKV) entry(d *structs.VariableDecrypted) (*kvEntry, error) {
	key, err := decodeKVKey(strings.TrimPrefix(d.Path, v.path+"/"))
	if err != nil {
		return nil, err
	}
	e := &kvEntry{Key: key, CreateIndex: d.CreateIndex, ModifyIndex: d.ModifyIndex}
	if raw, ok := d.Items[kvItemValueBase64]; ok {
		if e.Value, err = base64.StdEncoding.DecodeString(raw); err != nil {
			return nil, err
		}
	} else if raw, ok := d.Items[kvItemValue]; ok {
		e.Value = []byte(raw)
	}
	if raw := d.Items[kvItemFlags]; raw != "" {
		if e.Flags, err = strconv.ParseUint(raw, 10, 64); err != nil {
			return nil, err
		}
	}
	return e, nil
}

func (v *variablesKV) Put(r *request, e *kvEntry, cas *uint64) (bool, error) {
	path, err := v.varPath(e.Key)
	if err != nil {
		return false, err
	}
	items := structs.VariableItems{}
	if utf8.Valid(e.Value) {
		items[kvItemValue] = string(e.Value)
	} else {
		items[kvItemValueBase64] = base64.StdEncoding.EncodeToString(e.Value)
	}
	if e.Flags != 0 {
		items[kvItemFlags] = strconv.FormatUint(e.Flags, 10)
	}
	vd := &structs.VariableDecrypted{
		VariableMetadata: structs.VariableMetadata{Namespace: v.namespace, Path: path},
		Items:            items,
	}
	op := structs.VarOpSet
	if cas != nil {
		op = structs.VarOpCAS
		vd.ModifyIndex = *cas
	}
	return v.apply(r, op, vd)
}

func (v *variablesKV) Delete(r *request, key string, recurse bool, cas *uint64) (bool, error) {
	if !recurse {
		path, err := v.varPath(key)
		if err != nil {
			return false, err
		}
		vd := &structs.VariableDecrypted{VariableMetadata: structs.VariableMetadata{Namespace: v.namespace, Path: path}}
		op := structs.VarOpDelete
		if cas != nil {
			op = structs.VarOpDeleteCAS
			vd.ModifyIndex = *cas
		}
		return v.apply(r, op, vd)
	}

	// Not atomic: the keys are deleted one by one.
	nr := *r
	nr.index = 0
	entries, _, err := v.Get(&nr, key, true)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if _, err := v.Delete(r, e.Key, false, nil); err != nil {
			return false, err
		}
	}
	return true, nil
}

// apply writes a Variable; a failed check-and-set is false.
func (v *variablesKV) apply(r *request, op structs.VarOp, vd *structs.VariableDecrypted) (bool, error) {
	args := &structs.VariablesApplyRequest{
		Op:  op,
		Var: vd,
		WriteRequest: structs.WriteRequest{
			Region:    r.region,
			Namespace: v.namespace,
			AuthToken: r.token,
		},
	}
	var resp structs.VariablesApplyResponse
	if err := v.backend.RPC(structs.VariablesApplyRPCMethod, args, &resp); err != nil {
		return false, err
	}
	if resp.IsConflict() {
		return false, nil
	}
	if resp.Error != nil {
		return false, resp.Error
	}
	return true, nil
}

func (v *variablesKV) Close() {}
