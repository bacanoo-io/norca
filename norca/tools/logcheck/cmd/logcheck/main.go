// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Command logcheck runs the logcheck analyzer: on its own (logcheck ./...,
// with -fix to apply the suggested fixes) or as the tool of go vet
// (go vet -vettool=$(which logcheck) ./...).
package main

import (
	"github.com/bacanoo-io/norca/norca/tools/logcheck"
	"golang.org/x/tools/go/analysis/singlechecker"
)

func main() {
	singlechecker.Main(logcheck.Analyzer)
}
