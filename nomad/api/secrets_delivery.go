// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import "time"

// TaskSecret is a secret block of a task: a secret of the built-in store or
// of an external provider, delivered into the task's secrets directory
// and/or environment. Jobs only reference secrets, they never carry values.
//
//	secret "db" {
//	  path = "prod/db/postgres"
//	  file {
//	    destination = "secrets/db.env"
//	    format      = "env"
//	  }
//	  env {
//	    DB_PASSWORD = "password"
//	  }
//	}
type TaskSecret struct {
	// Name is the label of the block.
	Name string `hcl:"name,label"`

	// Provider names the provider; empty resolves it by the client routes,
	// the job, group and namespace defaults, falling back to the built-in
	// store ("norca").
	Provider string `hcl:"provider,optional" json:",omitempty"`

	// Path is the provider-specific path of the secret; ${job}, ${group}
	// and ${task} are replaced.
	Path string `hcl:"path,optional"`

	// Version pins a version; 0 is the latest.
	Version uint64 `hcl:"version,optional" json:",omitempty"`

	// Engine and Role ask for a dynamic secret: a credential of the role of
	// the engine of the job's namespace, delivered to the engine's default
	// file (secrets/nats.creds for nats) unless a file or env block is
	// given. Path and Version are empty then.
	Engine string `hcl:"engine,optional" json:",omitempty"`
	Role   string `hcl:"role,optional" json:",omitempty"`

	// Params are passed to the operator of a dynamic secret.
	Params map[string]string `hcl:"params,optional" json:",omitempty"`

	// Fallback is a path of the built-in store delivered when a dynamic
	// secret can't be created at the start of the task.
	Fallback        string `hcl:"fallback,optional" json:",omitempty"`
	FallbackRecover string `hcl:"fallback_recover,optional" json:",omitempty"`

	// FallbackRecoverInterval is how often fallback_recover asks for a
	// credential (default 10s).
	FallbackRecoverInterval time.Duration `hcl:"fallback_recover_interval,optional" json:",omitempty"`

	// FallbackKeepOnRestart keeps a task on its fallback when it restarts;
	// otherwise a restart asks for a credential again.
	FallbackKeepOnRestart bool `hcl:"fallback_keep_on_restart,optional" json:",omitempty"`

	// Files are the files the secret is delivered to.
	Files []*TaskSecretFile `hcl:"file,block" json:",omitempty"`

	// Env maps environment variables to items.
	Env map[string]string `hcl:"env,block" json:",omitempty"`

	// ChangeMode is restart (default), signal, script or noop.
	ChangeMode   string        `mapstructure:"change_mode" hcl:"change_mode,optional" json:",omitempty"`
	ChangeSignal string        `mapstructure:"change_signal" hcl:"change_signal,optional" json:",omitempty"`
	ChangeScript *ChangeScript `mapstructure:"change_script" hcl:"change_script,block" json:",omitempty"`

	// Optional makes the secret non-blocking for the task start.
	Optional bool `hcl:"optional,optional" json:",omitempty"`

	// Timeout bounds the wait for the first delivery (default 5m).
	Timeout time.Duration `hcl:"timeout,optional" json:",omitempty"`
}

// TaskSecretFile is the file a secret is delivered to.
type TaskSecretFile struct {
	// Destination is below secrets/, relative to the task directory.
	Destination string `hcl:"destination"`

	// Format is env (default), json, raw:<item> or template.
	Format string `hcl:"format,optional" json:",omitempty"`

	// Template renders the file for the template format; the items are its
	// data.
	Template string `hcl:"template,optional" json:",omitempty"`
}

// SecretAccess is the secret_access shorthand binding of a group or task:
// read-secret on matching secret paths for the tasks' workload identities.
// The submitter of the job must hold read-secret on the pattern.
type SecretAccess struct {
	Path string `hcl:"path"`
}

// SecretsConfig is the secrets block of a job or group.
type SecretsConfig struct {
	DefaultProvider string `mapstructure:"default_provider" hcl:"default_provider,optional" json:",omitempty"`

	// NamespaceDefaultProvider is set by the servers on the job's block
	// from the namespace's "secrets.default_provider" metadata.
	NamespaceDefaultProvider string `json:",omitempty"`
}
