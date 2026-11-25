// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"io"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestRiggerArgs(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		compLine string
		expected []string
		ok       bool
	}{
		{name: "no arguments", args: nil},
		{name: "another command", args: []string{"job", "run", "-h"}},
		{name: "rigger later on the line", args: []string{"-h", "rigger"}},
		{name: "rigger alone", args: []string{"rigger"}, expected: []string{}, ok: true},
		{name: "a command of rigger with -h", args: []string{"rigger", "stack", "apply", "-h"}, expected: []string{"stack", "apply", "-h"}, ok: true},
		{name: "a completion request", args: []string{"rigger", "st"}, compLine: "norca rigger st"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setCompLine(t, tt.compLine)

			got, ok := riggerArgs(tt.args)
			if ok != tt.ok || !slices.Equal(got, tt.expected) {
				t.Errorf("riggerArgs(%q) = %q, %v; want %q, %v", tt.args, got, ok, tt.expected, tt.ok)
			}
		})
	}
}

// TestRun_RiggerCommandHelp: -h after a command of Rigger prints the help of
// that command, not the help of "norca rigger".
func TestRun_RiggerCommandHelp(t *testing.T) {
	setCompLine(t, "")

	out := captureStdout(t, func() {
		if code := Run([]string{"rigger", "search", "-h"}); code != 0 {
			t.Errorf("Run() = %d, want 0", code)
		}
	})

	for _, want := range []string{"Usage:\n  norca rigger search", "--max-width"} {
		if !strings.Contains(out, want) {
			t.Errorf("the help does not contain %q:\n%s", want, out)
		}
	}

	if strings.Contains(out, "Available Commands") {
		t.Errorf("the help is the help of norca rigger, not of rigger search:\n%s", out)
	}
}

// setCompLine sets COMP_LINE for the test, or unsets it when line is empty.
func setCompLine(t *testing.T, line string) {
	t.Helper()

	t.Setenv("COMP_LINE", line)

	if line == "" {
		if err := os.Unsetenv("COMP_LINE"); err != nil {
			t.Fatal(err)
		}
	}
}

// captureStdout returns what fn writes to os.Stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	stdout := os.Stdout
	os.Stdout = w

	done := make(chan string)

	go func() {
		b, err := io.ReadAll(r)
		if err != nil {
			t.Error(err)
		}

		done <- string(b)
	}()

	fn()

	os.Stdout = stdout

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	return <-done
}
