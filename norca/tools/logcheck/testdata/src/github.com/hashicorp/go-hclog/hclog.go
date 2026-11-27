// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package hclog is a stub of github.com/hashicorp/go-hclog for the tests.
package hclog

import "fmt"

type Level int

type Format []any

func Fmt(str string, args ...any) Format { return append(Format{str}, args...) }

type CapturedStacktrace string

func Stacktrace() CapturedStacktrace { return "" }

type Logger interface {
	Log(level Level, msg string, args ...any)
	Trace(msg string, args ...any)
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
	With(args ...any) Logger
	Named(name string) Logger
}

type InterceptLogger interface {
	Logger
	NamedIntercept(name string) InterceptLogger
}

func New() Logger { return nil }

var _ = fmt.Sprint
