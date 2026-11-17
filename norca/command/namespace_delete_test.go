// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/mitchellh/cli"
	"github.com/posener/complete"
	"github.com/shoenig/test/must"
	"github.com/stretchr/testify/assert"
)

func TestNamespaceDeleteCommand_Implements(t *testing.T) {
	ci.Parallel(t)
	var _ cli.Command = &NamespaceDeleteCommand{}
}

func TestNamespaceDeleteCommand_Fails(t *testing.T) {
	ci.Parallel(t)
	ui := cli.NewMockUi()
	cmd := &NamespaceDeleteCommand{Meta: Meta{Ui: ui}}

	// Fails on misuse
	if code := cmd.Run([]string{"some", "bad", "args"}); code != 1 {
		t.Fatalf("expected exit code 1, got: %d", code)
	}
	if out := ui.ErrorWriter.String(); !strings.Contains(out, commandErrorText(cmd)) {
		t.Fatalf("expected help output, got: %s", out)
	}
	ui.ErrorWriter.Reset()

	if code := cmd.Run([]string{"-address=nope", "foo"}); code != 1 {
		t.Fatalf("expected exit code 1, got: %d", code)
	}
	if out := ui.ErrorWriter.String(); !strings.Contains(out, "deleting namespace") {
		t.Fatalf("connection error, got: %s", out)
	}
	ui.ErrorWriter.Reset()
}

func TestNamespaceDeleteCommand_Good(t *testing.T) {
	ci.Parallel(t)

	// Create a server
	srv, client, url := testServer(t, true, nil)
	defer srv.Shutdown()

	ui := cli.NewMockUi()
	cmd := &NamespaceDeleteCommand{Meta: Meta{Ui: ui}}

	// Create a namespace to delete
	ns := &api.Namespace{
		Name: "foo",
	}
	_, err := client.Namespaces().Register(ns, nil)
	assert.Nil(t, err)

	// Delete a namespace
	if code := cmd.Run([]string{"-address=" + url, ns.Name}); code != 0 {
		t.Fatalf("expected exit 0, got: %d; %v", code, ui.ErrorWriter.String())
	}

	namespaces, _, err := client.Namespaces().List(nil)
	assert.Nil(t, err)
	assert.Len(t, namespaces, 1)
}

func TestNamespaceDeleteCommand_AutocompleteArgs(t *testing.T) {
	ci.Parallel(t)
	assert := assert.New(t)

	srv, client, url := testServer(t, true, nil)
	defer srv.Shutdown()

	ui := cli.NewMockUi()
	cmd := &NamespaceDeleteCommand{Meta: Meta{Ui: ui, flagAddress: url}}

	// Create a namespace other than default
	ns := &api.Namespace{
		Name: "diddo",
	}
	_, err := client.Namespaces().Register(ns, nil)
	assert.Nil(err)

	args := complete.Args{Last: "d"}
	predictor := cmd.AutocompleteArgs()

	res := predictor.Predict(args)
	assert.Equal(1, len(res))
	assert.Equal(ns.Name, res[0])
}

// A deletion the servers refuse fails with the reason, and one of a
// namespace that does not exist says so (review finding SEC-13).
func TestNamespaceDeleteCommand_Refused(t *testing.T) {
	ci.Parallel(t)

	srv, client, url := testServer(t, true, nil)
	defer srv.Shutdown()

	ns := &api.Namespace{Name: "held"}
	_, err := client.Namespaces().Register(ns, nil)
	must.NoError(t, err)
	_, _, err = client.Variables().Create(&api.Variable{
		Namespace: ns.Name, Path: "app/var", Items: api.VariableItems{"k": "v"},
	}, &api.WriteOptions{Namespace: ns.Name})
	must.NoError(t, err)

	ui := cli.NewMockUi()
	cmd := &NamespaceDeleteCommand{Meta: Meta{Ui: ui}}
	code := cmd.Run([]string{"-address=" + url, ns.Name})
	must.Eq(t, 1, code, must.Sprintf("the refused deletion reported success: %s", ui.OutputWriter.String()))
	must.StrContains(t, ui.ErrorWriter.String(), "contains at least one variable")
	must.StrNotContains(t, ui.OutputWriter.String(), "Successfully deleted")
	out, _, err := client.Namespaces().Info(ns.Name, nil)
	must.NoError(t, err)
	must.NotNil(t, out)

	ui = cli.NewMockUi()
	cmd = &NamespaceDeleteCommand{Meta: Meta{Ui: ui}}
	code = cmd.Run([]string{"-address=" + url, "no-such-namespace"})
	must.Eq(t, 1, code)
	must.StrContains(t, ui.ErrorWriter.String(), `namespace "no-such-namespace" not found`)
	must.StrContains(t, ui.ErrorWriter.String(), "404")
}
