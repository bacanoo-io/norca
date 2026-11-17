// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package controller_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/controller"
	"github.com/bacanoo-io/norca/norca/controller/fake"
	"github.com/shoenig/test/must"
)

// resetVars is a Variables API whose state is replaced by a server that
// started again without its data: the same object exists with a lower
// index.
type resetVars struct {
	controller.VariablesAPI

	mu      sync.Mutex
	index   uint64
	widget  *api.Variable
	restart chan struct{}
}

func (r *resetVars) state() (uint64, *api.Variable) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.index, r.widget.Copy()
}

func (r *resetVars) ListVariables(ctx context.Context, _, _ string, waitIndex uint64) ([]*api.VariableMetadata, uint64, error) {
	if index, _ := r.state(); waitIndex >= index {
		// A blocking query: it returns when the server came back.
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-r.restart:
		}
		if index, _ := r.state(); waitIndex == index {
			<-ctx.Done()
			return nil, 0, ctx.Err()
		}
	}
	index, v := r.state()
	return []*api.VariableMetadata{v.Metadata()}, index, nil
}

func (r *resetVars) ReadVariable(_ context.Context, _, path string) (*api.Variable, error) {
	_, v := r.state()
	if v.Path != path {
		return nil, fmt.Errorf("variable %s: %w", path, controller.ErrNotFound)
	}
	return v, nil
}

func TestInformer_IndexWentBack(t *testing.T) {
	widget := func(index uint64, spec string) *api.Variable {
		return &api.Variable{
			Namespace: "default", Path: widgetKind.Path("a"),
			CreateIndex: index, ModifyIndex: index,
			Items: api.VariableItems{"spec": spec},
		}
	}
	vars := &resetVars{index: 100, widget: widget(90, `{"v":"before"}`), restart: make(chan struct{})}

	inf, err := controller.NewInformer(widgetKind, vars, fake.New(), controller.InformerOptions{
		WatchMode: controller.WatchPoll,
		Logger:    testLogger(t),
	})
	must.NoError(t, err)
	rec := &recorder{}
	inf.AddHandler(rec.handler())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		inf.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	must.True(t, inf.WaitForSync(ctx))
	obj, ok := inf.Get(controller.ObjectKey{Namespace: "default", Name: "a"})
	must.True(t, ok)
	must.Eq(t, `{"v":"before"}`, string(obj.Spec))

	// The server starts again and the object is created again.
	vars.mu.Lock()
	vars.index, vars.widget = 5, widget(3, `{"v":"after"}`)
	vars.mu.Unlock()
	close(vars.restart)

	waitFor(t, "the cache follows the listing", func() bool {
		obj, ok := inf.Get(controller.ObjectKey{Namespace: "default", Name: "a"})
		return ok && string(obj.Spec) == `{"v":"after"}` && obj.ModifyIndex == 3
	})
	must.Eq(t, []string{"add a", "update a"}, rec.get())
}
