// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"bytes"
	"net/url"
	"time"
)

// RiggerBundle is a bundle of Rigger kept in the cluster: the modules of a
// stack from their git sources, for a cluster without a network.
type RiggerBundle struct {
	Name  string
	Stack string
	// Modules are "source version" of each module of the bundle.
	Modules  []string
	Size     uint64
	Imported time.Time
}

// ImportBundle sends a bundle (the archive of rigger bundle create) to the
// cluster, which checks its digests and keeps it.
func (r *Rigger) ImportBundle(data []byte, w *WriteOptions) (*RiggerBundle, *WriteMeta, error) {
	var out RiggerBundle

	wm, err := r.client.put("/v1/rigger/bundle", bytes.NewReader(data), &out, w)
	if err != nil {
		return nil, nil, err
	}

	return &out, wm, nil
}

// Bundles lists the bundles kept in the cluster.
func (r *Rigger) Bundles(q *QueryOptions) ([]*RiggerBundle, *QueryMeta, error) {
	var out []*RiggerBundle

	qm, err := r.client.query("/v1/rigger/bundles", &out, q)
	if err != nil {
		return nil, nil, err
	}

	return out, qm, nil
}

// DeleteBundle removes a bundle from the cluster.
func (r *Rigger) DeleteBundle(name string, w *WriteOptions) (*WriteMeta, error) {
	return r.client.delete("/v1/rigger/bundle/"+url.PathEscape(name), nil, nil, w)
}
