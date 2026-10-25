// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package fake

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/controller"
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
