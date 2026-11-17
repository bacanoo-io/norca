// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"encoding/json"
	"testing"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/shoenig/test/must"
)

// An object of a cluster-scoped kind has no namespace: its owner reference
// starts with the "/", and the server writes the same value.
func TestOwnerFromMeta_ClusterScoped(t *testing.T) {
	kind := Kind{Group: "example.com", Kind: "Cluster"}
	obj := &Object{Kind: kind, Key: ObjectKey{Name: "c1"}}
	job := &api.Job{}
	SetOwner(job, obj)
	must.Eq(t, "/crd/example-com/Cluster/c1", job.Meta[OwnerMetaKey])
	must.True(t, IsOwnedBy(job.Meta, obj))

	key, ok := OwnerFromMeta(kind, job.Meta)
	must.True(t, ok)
	must.Eq(t, ObjectKey{Name: "c1"}, key)

	// A value without any "/" is still not an owner reference.
	_, ok = OwnerFromMeta(kind, map[string]string{OwnerMetaKey: "c1"})
	must.False(t, ok)
}

// The changes of the jobs of a cluster-scoped object reach the owner.
func TestOwnedJobsSource_ClusterScopedOwner(t *testing.T) {
	kind := Kind{Group: "example.com", Kind: "Cluster"}
	obj := &Object{Kind: kind, Key: ObjectKey{Name: "c1"}}
	job := &api.Job{ID: pointerOf("j"), Namespace: pointerOf("default")}
	SetOwner(job, obj)

	var got []ObjectKey
	sc := &SourceContext{kind: kind, Enqueue: func(k ObjectKey) { got = append(got, k) }}
	src := &OwnedJobsSource{owned: map[string]ownedJob{}}
	raw, err := json.Marshal(map[string]any{"Job": job})
	must.NoError(t, err)
	var payload map[string]any
	must.NoError(t, json.Unmarshal(raw, &payload))
	src.apply(&api.Event{Topic: api.TopicJob, Namespace: "default", Payload: payload}, sc)
	must.Eq(t, []ObjectKey{{Name: "c1"}}, got)

	owner, ok := src.lookup("default", "j")
	must.True(t, ok)
	must.Eq(t, ObjectKey{Name: "c1"}, owner)
}

func pointerOf[T any](v T) *T { return &v }
