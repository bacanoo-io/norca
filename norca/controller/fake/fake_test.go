// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package fake

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/controller"
	"github.com/shoenig/test/must"
)

func recv(t *testing.T, ch <-chan *api.Events) *api.Events {
	t.Helper()
	select {
	case evs := <-ch:
		return evs
	case <-time.After(2 * time.Second):
		t.Fatal("no events")
		return nil
	}
}

func TestBackend_StreamExact(t *testing.T) {
	b := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	v := b.PutVariable(&api.Variable{Path: "crd/x/K/a", Items: api.VariableItems{"spec": "{}"}})
	topics := map[api.Topic][]string{api.TopicVariables: {"crd/x/K/*"}}

	ch, err := b.Stream(ctx, "default", topics, v.ModifyIndex, true)
	must.NoError(t, err)
	evs := recv(t, ch)
	must.Eq(t, v.ModifyIndex, evs.Index)
	meta, err := evs.Events[0].Variable()
	must.NoError(t, err)
	must.Eq(t, "crd/x/K/a", meta.Path)

	b.PutVariable(&api.Variable{Path: "other/a", Items: api.VariableItems{"a": "b"}})
	v2 := b.PutVariable(&api.Variable{Path: "crd/x/K/b", Items: api.VariableItems{"spec": "{}"}})
	evs = recv(t, ch)
	must.Eq(t, v2.ModifyIndex, evs.Index)

	b.TruncateEvents(b.Index() + 1)
	evs = recv(t, ch)
	must.Error(t, evs.Err)
	_, err = b.Stream(ctx, "default", topics, v2.ModifyIndex, true)
	must.ErrorIs(t, err, api.ErrEventIndexGone)
}

func TestBackend_VariablesCAS(t *testing.T) {
	b := New()
	ctx := context.Background()
	v, err := b.WriteVariable(ctx, &api.Variable{Path: "a", Items: api.VariableItems{"k": "v"}})
	must.NoError(t, err)
	_, err = b.WriteVariable(ctx, &api.Variable{Path: "a", Items: api.VariableItems{"k": "v"}})
	must.ErrorIs(t, err, controller.ErrConflict)
	v2, err := b.WriteVariable(ctx, v)
	must.NoError(t, err)
	must.Greater(t, v.ModifyIndex, v2.ModifyIndex)
	must.ErrorIs(t, b.DeleteVariable(ctx, "default", "a", v.ModifyIndex), controller.ErrConflict)
	must.NoError(t, b.DeleteVariable(ctx, "default", "a", v2.ModifyIndex))
	_, err = b.ReadVariable(ctx, "default", "a")
	must.True(t, errors.Is(err, controller.ErrNotFound))
}

// TestBackend_ListEmptyThenStream lists a kind with no object, writes one, and
// starts the stream the way a watch does (from the list index + 1, exact): the
// object written between the list and the stream is delivered. A list of an
// empty table used to answer 0, so the watch streamed "from now" and missed
// it (about one run in 40 of the LoadBalancer operator tests with -race).
func TestBackend_ListEmptyThenStream(t *testing.T) {
	b := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	kind := controller.Kind{Group: "example.io", Kind: "Widget"}

	objs, idx, err := b.ListObjects(ctx, kind, "*", 0)
	must.NoError(t, err)
	must.SliceEmpty(t, objs)
	must.Positive(t, idx)

	o := b.PutObject(&api.CustomObject{Group: kind.Group, Kind: kind.Kind, Name: "a", Spec: map[string]any{}})
	ch, err := b.Stream(ctx, "*", map[api.Topic][]string{api.TopicCustomObject: {"*"}}, idx+1, true)
	must.NoError(t, err)
	evs := recv(t, ch)
	must.Eq(t, o.ModifyIndex, evs.Index)

	// The same for Variables and jobs.
	_, vidx, err := b.ListVariables(ctx, "*", "", 0)
	must.NoError(t, err)
	must.Positive(t, vidx)
	_, jidx, err := b.ListJobs(ctx, "*", 0)
	must.NoError(t, err)
	must.Positive(t, jidx)
}

func TestBackend_StreamFuture(t *testing.T) {
	b := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.PutVariable(&api.Variable{Path: "crd/x/K/a", Items: api.VariableItems{"spec": "{}"}})
	topics := map[api.Topic][]string{api.TopicVariables: {"crd/x/K/*"}}
	ch, err := b.Stream(ctx, "default", topics, b.Index()+1, true)
	must.NoError(t, err)
	v := b.PutVariable(&api.Variable{Path: "crd/x/K/b", Items: api.VariableItems{"spec": "{}"}})
	evs := recv(t, ch)
	must.Eq(t, v.ModifyIndex, evs.Index)
}
