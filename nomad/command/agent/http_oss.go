// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

//go:build !ent
// +build !ent

package agent

import (
	"net/http"
)

// auditHandler wraps the passed handlerFn
func (s *HTTPServer) auditHandler(h handlerFn) handlerFn {
	return h
}

// auditHTTPHandler wraps  the passed handlerByteFn
func (s *HTTPServer) auditNonJSONHandler(h handlerByteFn) handlerByteFn {
	return h
}

// auditHTTPHandler wraps the passed http.Handler
func (s *HTTPServer) auditHTTPHandler(h http.Handler) http.Handler {
	return h
}
