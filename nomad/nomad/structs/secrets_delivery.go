// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"errors"
	"fmt"
	"maps"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	multierror "github.com/hashicorp/go-multierror"
)

// Delivery of secrets to tasks (norca-doc content/docs/design/secrets-plan.md, "Delivery to
// tasks" and "Provider resolution and fallback"). A task's secret blocks name
// a secret by path; the client resolves the provider that answers the lookup,
// fetches the value with the task's workload identity and delivers it into
// the task's secrets directory and/or environment. Jobs only ever reference
// secrets: values are never part of a job, an allocation or task events.

const (
	// SecretProviderNorca is the built-in secret store, the fallback
	// provider of every lookup.
	SecretProviderNorca = "norca"

	// SecretProviderVault is the agent's existing Vault integration, defined
	// implicitly when the agent's vault block is enabled.
	SecretProviderVault = "vault"

	// SecretChangeMode* are the actions taken when a delivered secret
	// changes.
	SecretChangeModeRestart = "restart"
	SecretChangeModeSignal  = "signal"
	SecretChangeModeScript  = "script"
	SecretChangeModeNoop    = "noop"

	// SecretFileFormat* are the formats of a secret file. A raw file holds
	// the value of one item: "raw:<item>".
	SecretFileFormatEnv      = "env"
	SecretFileFormatJSON     = "json"
	SecretFileFormatRaw      = "raw"
	SecretFileFormatTemplate = "template"

	// DefaultSecretTimeout is how long the start of a task waits for a
	// secret to be delivered unless the secret block sets a timeout.
	DefaultSecretTimeout = 5 * time.Minute

	// NamespaceMetaSecretsDefaultProvider is the namespace metadata key of
	// the default secret provider of the namespace (resolution step 3). It
	// is captured on the job at registration, like the namespace's default
	// node pool.
	NamespaceMetaSecretsDefaultProvider = "secrets.default_provider"

	// SecretPlaceholder* can be used in secret paths and secret_access
	// paths; they are replaced with the job ID (the parent's ID for
	// dispatched and periodic children), the task group and the task name.
	SecretPlaceholderJob   = "${job}"
	SecretPlaceholderGroup = "${group}"
	SecretPlaceholderTask  = "${task}"

	// secretsDirPrefix is the task directory every secret file is written
	// below.
	secretsDirPrefix = "secrets/"
)

var (
	// validSecretProviderName restricts provider names.
	validSecretProviderName = regexp.MustCompile("^[a-zA-Z0-9_-]{1,64}$")

	// validTaskSecretName restricts the label of a secret block.
	validTaskSecretName = regexp.MustCompile("^[a-zA-Z0-9_-]{1,128}$")

	// validSecretEnvName restricts the environment variables a secret is
	// delivered to.
	validSecretEnvName = regexp.MustCompile("^[A-Za-z_][A-Za-z0-9_]*$")

	// validSecretAccessPath is a secret path pattern: secret path characters
	// and globs, after the placeholders are removed.
	validSecretAccessPath = regexp.MustCompile("^[a-zA-Z0-9-_~/*]{1,256}$")

	// reservedSecretFiles are written by other hooks into the secrets
	// directory.
	reservedSecretFiles = map[string]struct{}{
		"secrets/nomad_token": {},
		"secrets/vault_token": {},
	}
)

// TaskSecret is a secret block of a task: one secret delivered into the
// task's secrets directory and/or environment.
type TaskSecret struct {
	// Name is the label of the block, unique within the task.
	Name string

	// Provider names the provider explicitly (resolution step 1); empty
	// resolves the provider by the client routes, the job, group and
	// namespace defaults, and falls back to the built-in store.
	Provider string

	// Path is the provider-specific path of the secret. It may contain the
	// ${job}, ${group} and ${task} placeholders.
	Path string

	// Version pins a version of the secret; 0 is the latest version, which
	// is re-delivered when it changes.
	Version uint64

	// Engine and Role are reserved for dynamic secrets (a later phase);
	// they must be empty for now.
	Engine string
	Role   string

	// File delivers the secret as a file below secrets/.
	File *TaskSecretFile

	// Env delivers items as environment variables: variable name to item.
	Env map[string]string

	// ChangeMode, ChangeSignal and ChangeScript say what happens when the
	// secret changes, as for templates.
	ChangeMode   string
	ChangeSignal string
	ChangeScript *ChangeScript

	// Optional makes the secret non-blocking: the task starts without it if
	// it can't be delivered.
	Optional bool

	// Timeout bounds the wait for the first delivery before the task fails.
	Timeout time.Duration
}

// TaskSecretFile is the file a secret is delivered to.
type TaskSecretFile struct {
	// Destination is the file below secrets/, relative to the task
	// directory.
	Destination string

	// Format is env, json, raw:<item> or template.
	Format string

	// Template is the Go template rendering the file for the template
	// format; the items are its data ({{ .password }}).
	Template string
}

// SecretAccess is the secret_access shorthand binding of a task group or
// task: it grants the workload identities of the tasks read-secret on the
// secret paths matching Path in the job's namespace. The submitter of the job
// must hold read-secret on the pattern itself; a job can't grant itself
// access the submitter doesn't have.
type SecretAccess struct {
	// Path is a secret path pattern with globs and the ${job}, ${group}
	// and ${task} placeholders.
	Path string
}

// SecretsConfig is the secrets block of a job or task group: the default
// provider of the lookups of its tasks (resolution step 3).
type SecretsConfig struct {
	// DefaultProvider is the provider used when neither the secret nor a
	// client route names one.
	DefaultProvider string

	// NamespaceDefaultProvider is the default provider of the job's
	// namespace (NamespaceMetaSecretsDefaultProvider), set by the servers
	// on the job's block at registration. It is ignored when submitted and
	// in groups.
	NamespaceDefaultProvider string
}

// Copy returns a deep copy of the secret block.
func (s *TaskSecret) Copy() *TaskSecret {
	if s == nil {
		return nil
	}
	out := *s
	out.File = s.File.Copy()
	out.Env = maps.Clone(s.Env)
	out.ChangeScript = s.ChangeScript.Copy()
	return &out
}

// Equal compares two secret blocks.
func (s *TaskSecret) Equal(o *TaskSecret) bool {
	if s == nil || o == nil {
		return s == o
	}
	switch {
	case s.Name != o.Name, s.Provider != o.Provider, s.Path != o.Path, s.Version != o.Version,
		s.Engine != o.Engine, s.Role != o.Role,
		s.ChangeMode != o.ChangeMode, s.ChangeSignal != o.ChangeSignal,
		s.Optional != o.Optional, s.Timeout != o.Timeout:
		return false
	case !s.File.Equal(o.File), !maps.Equal(s.Env, o.Env), !s.ChangeScript.Equal(o.ChangeScript):
		return false
	}
	return true
}

// Copy returns a copy of the file block.
func (f *TaskSecretFile) Copy() *TaskSecretFile {
	if f == nil {
		return nil
	}
	out := *f
	return &out
}

// Equal compares two file blocks.
func (f *TaskSecretFile) Equal(o *TaskSecretFile) bool {
	if f == nil || o == nil {
		return f == o
	}
	return *f == *o
}

// RawItem returns the item of a raw:<item> format, or "".
func (f *TaskSecretFile) RawItem() string {
	if f == nil {
		return ""
	}
	if item, ok := strings.CutPrefix(f.Format, SecretFileFormatRaw+":"); ok {
		return item
	}
	return ""
}

// Copy returns a copy of the binding.
func (a *SecretAccess) Copy() *SecretAccess {
	if a == nil {
		return nil
	}
	out := *a
	return &out
}

// Equal compares two bindings.
func (a *SecretAccess) Equal(o *SecretAccess) bool {
	if a == nil || o == nil {
		return a == o
	}
	return *a == *o
}

// Copy returns a copy of the secrets block.
func (c *SecretsConfig) Copy() *SecretsConfig {
	if c == nil {
		return nil
	}
	out := *c
	return &out
}

// Equal compares two secrets blocks.
func (c *SecretsConfig) Equal(o *SecretsConfig) bool {
	if c == nil || o == nil {
		return c == o
	}
	return *c == *o
}

// Validate checks a secrets block.
func (c *SecretsConfig) Validate() error {
	if c == nil {
		return nil
	}
	if c.DefaultProvider != "" && !validSecretProviderName.MatchString(c.DefaultProvider) {
		return fmt.Errorf("invalid secrets default_provider %q", c.DefaultProvider)
	}
	return nil
}

func copySecretAccess(in []*SecretAccess) []*SecretAccess {
	if in == nil {
		return nil
	}
	out := make([]*SecretAccess, len(in))
	for i, a := range in {
		out[i] = a.Copy()
	}
	return out
}

func copyTaskSecrets(in []*TaskSecret) []*TaskSecret {
	if in == nil {
		return nil
	}
	out := make([]*TaskSecret, len(in))
	for i, s := range in {
		out[i] = s.Copy()
	}
	return out
}

func secretAccessEqual(a, b []*SecretAccess) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].Equal(b[i]) {
			return false
		}
	}
	return true
}

// TaskSecretsEqual compares the secret blocks of two tasks.
func TaskSecretsEqual(a, b []*TaskSecret) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].Equal(b[i]) {
			return false
		}
	}
	return true
}

// SecretAccessEqual compares two lists of secret_access bindings.
func SecretAccessEqual(a, b []*SecretAccess) bool {
	return secretAccessEqual(a, b)
}

// Canonicalize sets the defaults of a secret block.
func (s *TaskSecret) Canonicalize() {
	if s.ChangeMode == "" {
		s.ChangeMode = SecretChangeModeRestart
	}
	if s.Timeout == 0 {
		s.Timeout = DefaultSecretTimeout
	}
	if s.File != nil && s.File.Format == "" {
		s.File.Format = SecretFileFormatEnv
	}
	if len(s.Env) == 0 {
		s.Env = nil
	}
	if s.ChangeScript != nil && s.ChangeScript.Args == nil {
		s.ChangeScript.Args = []string{}
	}
}

// Validate checks a secret block.
func (s *TaskSecret) Validate() error {
	var mErr *multierror.Error
	add := func(format string, args ...any) {
		mErr = multierror.Append(mErr, fmt.Errorf(format, args...))
	}

	if !validTaskSecretName.MatchString(s.Name) {
		add("invalid secret name %q", s.Name)
	}
	if s.Provider != "" && !validSecretProviderName.MatchString(s.Provider) {
		add("invalid provider name %q", s.Provider)
	}
	if s.Engine != "" || s.Role != "" {
		add("dynamic secret engines (engine, role) are not supported yet")
	}
	if err := validateSecretRefPath(s.Path); err != nil {
		mErr = multierror.Append(mErr, err)
	} else if s.Provider == SecretProviderNorca {
		// The built-in store's paths are checked here already; other
		// providers check their own paths.
		if err := ValidateSecretPath(placeholderFree(s.Path)); err != nil {
			mErr = multierror.Append(mErr, err)
		}
	}

	if s.File == nil && len(s.Env) == 0 {
		add("a secret needs a file or env block to be delivered")
	}
	if s.File != nil {
		if err := s.File.validate(); err != nil {
			mErr = multierror.Append(mErr, err)
		}
	}
	for name, item := range s.Env {
		if !validSecretEnvName.MatchString(name) {
			add("invalid environment variable name %q", name)
		} else if strings.HasPrefix(name, "NOMAD_") {
			add("environment variable %q: the NOMAD_ prefix is reserved", name)
		}
		if item == "" {
			add("environment variable %q needs an item", name)
		}
	}

	switch s.ChangeMode {
	case SecretChangeModeRestart, SecretChangeModeNoop:
	case SecretChangeModeSignal:
		if s.ChangeSignal == "" {
			add("change_mode signal requires a change_signal")
		}
	case SecretChangeModeScript:
		if s.ChangeScript == nil || s.ChangeScript.Command == "" {
			add("change_mode script requires a change_script with a command")
		} else if s.ChangeScript.Timeout < 0 {
			add("change_script timeout must not be negative")
		}
	default:
		add("invalid change_mode %q", s.ChangeMode)
	}
	if s.ChangeSignal != "" && s.ChangeMode != SecretChangeModeSignal {
		add("change_signal requires change_mode signal")
	}
	if s.Timeout < 0 {
		add("timeout must not be negative")
	}
	return mErr.ErrorOrNil()
}

func (f *TaskSecretFile) validate() error {
	dest := f.Destination
	clean := path.Clean(dest)
	switch {
	case dest == "":
		return errors.New("secret file requires a destination")
	case strings.Contains(dest, "\\") || path.IsAbs(dest) || clean != dest:
		return fmt.Errorf("secret file destination %q must be a clean relative path below secrets/", dest)
	case !strings.HasPrefix(clean, secretsDirPrefix) || len(clean) == len(secretsDirPrefix):
		return fmt.Errorf("secret file destination %q must be below secrets/", dest)
	}
	if _, ok := reservedSecretFiles[clean]; ok {
		return fmt.Errorf("secret file destination %q is reserved", dest)
	}

	switch {
	case f.Format == SecretFileFormatEnv, f.Format == SecretFileFormatJSON:
	case f.Format == SecretFileFormatTemplate:
		if f.Template == "" {
			return errors.New("secret file format template requires a template")
		}
		return nil
	case strings.HasPrefix(f.Format, SecretFileFormatRaw+":"):
		if f.RawItem() == "" {
			return fmt.Errorf("secret file format %q needs an item (raw:<item>)", f.Format)
		}
	default:
		return fmt.Errorf("invalid secret file format %q (env, json, raw:<item> or template)", f.Format)
	}
	if f.Template != "" {
		return errors.New("a secret file template requires format template")
	}
	return nil
}

// validateTaskSecrets checks the secret blocks and secret_access bindings of
// a task: names, destinations and environment variables are unique.
func validateTaskSecrets(t *Task) error {
	var mErr *multierror.Error
	names := map[string]struct{}{}
	dests := map[string]string{}
	envs := map[string]string{}
	for _, s := range t.Secrets {
		if s == nil {
			continue
		}
		if err := s.Validate(); err != nil {
			mErr = multierror.Append(mErr, multierror.Prefix(err, fmt.Sprintf("secret %q:", s.Name)))
		}
		if _, ok := names[s.Name]; ok {
			mErr = multierror.Append(mErr, fmt.Errorf("duplicate secret %q", s.Name))
		}
		names[s.Name] = struct{}{}
		if s.File != nil {
			if other, ok := dests[s.File.Destination]; ok {
				mErr = multierror.Append(mErr, fmt.Errorf("secrets %q and %q have the same destination", other, s.Name))
			}
			dests[s.File.Destination] = s.Name
			for _, tmpl := range t.Templates {
				if tmpl != nil && path.Clean(tmpl.DestPath) == s.File.Destination {
					mErr = multierror.Append(mErr, fmt.Errorf("secret %q has the destination of a template", s.Name))
				}
			}
		}
		for name := range s.Env {
			if other, ok := envs[name]; ok {
				mErr = multierror.Append(mErr, fmt.Errorf("secrets %q and %q set the same environment variable %q", other, s.Name, name))
			}
			envs[name] = s.Name
		}
	}
	if err := validateSecretAccess(t.SecretAccess, true); err != nil {
		mErr = multierror.Append(mErr, err)
	}
	return mErr.ErrorOrNil()
}

// validateSecretAccess checks secret_access bindings; ${task} is only
// allowed in bindings of tasks.
func validateSecretAccess(list []*SecretAccess, task bool) error {
	var mErr *multierror.Error
	for _, a := range list {
		if a == nil {
			continue
		}
		p := a.Path
		if !task && strings.Contains(p, SecretPlaceholderTask) {
			mErr = multierror.Append(mErr, fmt.Errorf("secret_access path %q: ${task} is only allowed in a task", p))
			continue
		}
		free := placeholderFree(p)
		if !validSecretAccessPath.MatchString(free) || strings.HasPrefix(free, "/") || strings.Contains(free, "//") {
			mErr = multierror.Append(mErr, fmt.Errorf("invalid secret_access path %q", p))
		}
	}
	return mErr.ErrorOrNil()
}

// validateSecretRefPath checks the path of a secret block: present, without
// unknown interpolations.
func validateSecretRefPath(p string) error {
	if p == "" {
		return errors.New("secret requires a path")
	}
	if len(p) > 512 || strings.TrimSpace(p) != p {
		return fmt.Errorf("invalid secret path %q", p)
	}
	if strings.Contains(placeholderFree(p), "$") {
		return fmt.Errorf("secret path %q: only the ${job}, ${group} and ${task} placeholders are supported", p)
	}
	return nil
}

// placeholderFree replaces the placeholders with a neutral path segment
// value, for validation.
func placeholderFree(p string) string {
	return strings.NewReplacer(SecretPlaceholderJob, "x", SecretPlaceholderGroup, "x",
		SecretPlaceholderTask, "x").Replace(p)
}

// SecretJobID is the job ID the ${job} placeholder stands for: the parent's
// ID for dispatched and periodic children, as for policies bound to jobs.
func SecretJobID(job *Job) string {
	if job == nil {
		return ""
	}
	if job.ParentID != "" {
		return job.ParentID
	}
	return job.ID
}

// InterpolateSecretPath replaces the ${job}, ${group} and ${task}
// placeholders of a secret path or secret_access path.
func InterpolateSecretPath(p, jobID, group, task string) string {
	if !strings.Contains(p, "${") {
		return p
	}
	return strings.NewReplacer(SecretPlaceholderJob, jobID, SecretPlaceholderGroup, group,
		SecretPlaceholderTask, task).Replace(p)
}

// SecretAccessPatterns returns the secret path patterns the secret_access
// bindings of the job grant to the given task (group bindings apply to all
// tasks of the group), with the placeholders replaced.
func SecretAccessPatterns(job *Job, group, task string) []string {
	if job == nil {
		return nil
	}
	tg := job.LookupTaskGroup(group)
	if tg == nil {
		return nil
	}
	jobID := SecretJobID(job)
	var out []string
	for _, a := range tg.SecretAccess {
		if a != nil {
			out = append(out, InterpolateSecretPath(a.Path, jobID, group, task))
		}
	}
	if t := tg.LookupTask(task); t != nil {
		for _, a := range t.SecretAccess {
			if a != nil {
				out = append(out, InterpolateSecretPath(a.Path, jobID, group, task))
			}
		}
	}
	return out
}

// SecretAccessGrant is a secret_access binding of a job with its
// placeholders replaced, for the authorization of the submitter.
type SecretAccessGrant struct {
	Group   string
	Task    string
	Pattern string
}

// SecretAccessGrants returns every grant of the job's secret_access
// bindings. Group bindings are returned once per task, since ${group} is the
// same for all of them.
func (j *Job) SecretAccessGrants() []SecretAccessGrant {
	var out []SecretAccessGrant
	jobID := SecretJobID(j)
	for _, tg := range j.TaskGroups {
		for _, a := range tg.SecretAccess {
			if a != nil {
				out = append(out, SecretAccessGrant{Group: tg.Name,
					Pattern: InterpolateSecretPath(a.Path, jobID, tg.Name, "")})
			}
		}
		for _, t := range tg.Tasks {
			for _, a := range t.SecretAccess {
				if a != nil {
					out = append(out, SecretAccessGrant{Group: tg.Name, Task: t.Name,
						Pattern: InterpolateSecretPath(a.Path, jobID, tg.Name, t.Name)})
				}
			}
		}
	}
	return out
}

// UsesSecretDelivery reports whether the job has secret blocks, secret_access
// bindings or secrets blocks, the jobspec features of secret delivery.
func (j *Job) UsesSecretDelivery() bool {
	if j.Secrets != nil {
		return true
	}
	for _, tg := range j.TaskGroups {
		if tg.Secrets != nil || len(tg.SecretAccess) > 0 {
			return true
		}
		for _, t := range tg.Tasks {
			if len(t.Secrets) > 0 || len(t.SecretAccess) > 0 {
				return true
			}
		}
	}
	return false
}

// SecretRef is a secret lookup referenced by a job: a secret block or a
// nomadSecret template call. It is what admission policies see of a job's
// secrets (the "secrets" list of the CEL request variable).
type SecretRef struct {
	Group string
	Task  string

	// Source is "secret" for a secret block and "template" for a
	// nomadSecret call in an embedded template.
	Source string

	// Name is the label of the secret block; empty for templates.
	Name string

	// Provider is the explicit provider ("" if the lookup is resolved by
	// the client), DefaultProvider the job, group or namespace default that
	// applies to it.
	Provider        string
	DefaultProvider string

	// Path is the path with the placeholders replaced.
	Path    string
	Version uint64

	// FileFormat is the format of the file delivery, "" without one.
	FileFormat string

	// Env are the environment variables the secret is delivered to.
	Env []string

	Optional bool
}

// nomadSecretCall matches the first argument of nomadSecret template calls.
var nomadSecretCall = regexp.MustCompile(`nomadSecret\s+"([^"]+)"`)

// ParseSecretRef splits the argument of a nomadSecret template call into the
// optional "provider:" prefix and the path.
func ParseSecretRef(ref string) (provider, p string) {
	if name, rest, ok := strings.Cut(ref, ":"); ok && rest != "" && validSecretProviderName.MatchString(name) {
		return name, rest
	}
	return "", ref
}

// SecretRefs returns the secret lookups of the job: its secret blocks and
// the nomadSecret calls found in embedded templates (templates read from a
// source file can't be inspected at submission).
func (j *Job) SecretRefs() []*SecretRef {
	var out []*SecretRef
	jobID := SecretJobID(j)
	for _, tg := range j.TaskGroups {
		def := j.SecretDefaultProvider(tg)
		for _, t := range tg.Tasks {
			for _, s := range t.Secrets {
				if s == nil {
					continue
				}
				ref := &SecretRef{
					Group:           tg.Name,
					Task:            t.Name,
					Source:          "secret",
					Name:            s.Name,
					Provider:        s.Provider,
					DefaultProvider: def,
					Path:            InterpolateSecretPath(s.Path, jobID, tg.Name, t.Name),
					Version:         s.Version,
					Optional:        s.Optional,
					Env:             []string{},
				}
				if s.File != nil {
					ref.FileFormat = s.File.Format
				}
				for name := range s.Env {
					ref.Env = append(ref.Env, name)
				}
				sort.Strings(ref.Env)
				out = append(out, ref)
			}
			for _, tmpl := range t.Templates {
				if tmpl == nil {
					continue
				}
				for _, m := range nomadSecretCall.FindAllStringSubmatch(tmpl.EmbeddedTmpl, -1) {
					provider, p := ParseSecretRef(m[1])
					out = append(out, &SecretRef{
						Group:           tg.Name,
						Task:            t.Name,
						Source:          "template",
						Provider:        provider,
						DefaultProvider: def,
						Path:            InterpolateSecretPath(p, jobID, tg.Name, t.Name),
						Env:             []string{},
					})
				}
			}
		}
	}
	return out
}

// SecretDefaultProvider returns the default provider of the lookups of a
// task group's tasks (resolution step 3): the group's, else the job's, else
// the namespace default captured on the job.
func (j *Job) SecretDefaultProvider(tg *TaskGroup) string {
	if tg != nil && tg.Secrets != nil && tg.Secrets.DefaultProvider != "" {
		return tg.Secrets.DefaultProvider
	}
	if j.Secrets != nil {
		if j.Secrets.DefaultProvider != "" {
			return j.Secrets.DefaultProvider
		}
		return j.Secrets.NamespaceDefaultProvider
	}
	return ""
}
