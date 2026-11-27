// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package a

import (
	"errors"
	"fmt"

	hclog "github.com/hashicorp/go-hclog"

	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// wrapper implements hclog.Logger by forwarding to another one.
type wrapper struct{ hclog.Logger }

func (w *wrapper) Info(msg string, args ...any) { w.Logger.Info(msg, args...) }

type notALogger struct{}

func (notALogger) Info(msg string, args ...any) {}

func messages(logger hclog.Logger, verb string, err error) {
	logger.Info("allocation started", "alloc_id", "1")
	logger.Info(fmt.Sprintf("failed to %s", verb))      // want `message built with fmt`
	logger.Debug("failed to " + verb)                   // want `message is not a constant`
	logger.Error(err.Error())                           // want `message is not a constant`
	logger.Warn("Failed to start the task")             // want `message starts with a capital letter`
	logger.Warn("failed to start the task.")            // want `message ends with a period`
	logger.Warn("RPC failed")                           // an acronym
	logger.Warn("UpsertJob failed")                     // an identifier
	logger.Warn("waiting...")                           // an ellipsis
	logger.Info("restarting in %s", "delay", 3)         // want `message holds a format verb`
	logger.Log(hclog.Level(2), "Stopped.", "task", "t") // want `message ends with a period and starts with a capital letter`
	logger.Info("periodic job", "job_id", "j")          //logcheck:ignore not checked
	//logcheck:ignore forwarded from a plugin
	logger.Info(verb)
	notALogger{}.Info(fmt.Sprintf("not %s", verb))
}

func forward(logger hclog.Logger, msg string, format string, args ...any) {
	logger.Info(msg, args...)
	logger.Debug(fmt.Sprintf(format, args...))
}

func keys(logger hclog.Logger, err error, key string) {
	logger.Info("odd", "alloc_id") // want `odd number of key/value arguments \(1\)`
	logger.Error("stack", "error", err, hclog.Stacktrace())
	logger.Info("key", key, 1)                  // want `key is not a constant string`
	logger.Info("camel", "allocID", "a")        // want `key "allocID" is not lower snake_case: use "alloc_id"`
	logger.Info("dotted", "plugin.type", "csi") // want `key "plugin.type" is not lower snake_case: use "plugin_type"`
	logger.Info("renamed", "err", err)          // want `key "err" is not the key of the convention: use "error"`
	logger.With("ns", "default", "tg", "web")   // want `key "ns" is not the key` `key "tg" is not the key`
	logger.Info("ambiguous", "node", "n1")      // want `key "node" is ambiguous: use node_id or node_name`
	logger.Info("reserved", "trace_id", "abc")  // want `key "trace_id" is reserved for helper/telemetry`
	logger.Info("error key", "reason", err)     // want `key "reason" holds an error`
	logger.Info("second error", "stat_error", err, "errors", errors.Join(err))
	logger.Info("twice", "node_id", "a", "node_id", "b") // want `key "node_id" is logged twice`
	w := &wrapper{logger}
	w.Info("via wrapper", "alloc", "a") // want `key "alloc" is not the key of the convention: use "alloc_id"`
}

func objects(logger hclog.Logger, alloc *structs.Allocation, job *structs.Job, token *structs.ACLToken, plan *structs.Plan) {
	logger.Info("alloc", "alloc", alloc)                 // want `key "alloc" is not the key` `a whole Allocation is logged: log its ID as "alloc_id"`
	logger.Info("job", "job_id", job.NamespacedID())     // want `a whole NamespacedID is logged`
	logger.Info("eval", "eval", hclog.Fmt("%#v", alloc)) // want `key "eval" is not the key` `a Allocation is formatted as a whole`
	logger.Info("token", "token", token.SecretID)        // want `SecretID is a secret and is never logged`
	logger.Trace("plan", "plan", hclog.Fmt("%#v", plan)) // a payload, not an object with an identity
	logger.Info("alloc id", "alloc_id", alloc.ID)
}
