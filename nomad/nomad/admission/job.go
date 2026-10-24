// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package admission implements Norca's dynamic admission control: CEL
// admission policies stored in Raft and external admission webhooks
// configured on the servers.
package admission

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/hashicorp/go-msgpack/codec"
	"github.com/hashicorp/nomad/nomad/structs"
)

// EncodeJob returns the job in the Nomad API JSON form (the same form the
// HTTP API returns) with its secrets removed.
func EncodeJob(job *structs.Job) ([]byte, error) {
	job = job.Copy()
	job.VaultToken = ""
	job.ConsulToken = ""

	var buf bytes.Buffer
	if err := codec.NewEncoder(&buf, structs.JsonHandle).Encode(job); err != nil {
		return nil, fmt.Errorf("failed to encode job: %w", err)
	}
	return buf.Bytes(), nil
}

// DecodeJob decodes a job from the JSON form produced by EncodeJob.
func DecodeJob(b []byte) (*structs.Job, error) {
	var job structs.Job
	if err := codec.NewDecoder(bytes.NewReader(b), structs.JsonHandle).Decode(&job); err != nil {
		return nil, fmt.Errorf("failed to decode job: %w", err)
	}
	return &job, nil
}

// JobValue converts the JSON form of a job into the generic value the CEL
// rules see as the "job" variable. Integers stay integers, so rules can
// compare them with integer literals.
func JobValue(b []byte) (any, error) {
	v, err := decodeJSON(b)
	if err != nil {
		return nil, err
	}
	return normalizeNumbers(v), nil
}

func normalizeNumbers(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			t[k] = normalizeNumbers(e)
		}
		return t
	case []any:
		for i, e := range t {
			t[i] = normalizeNumbers(e)
		}
		return t
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		if f, err := t.Float64(); err == nil {
			return f
		}
		return t.String()
	default:
		return v
	}
}
