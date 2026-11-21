// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package index is the format of the global registry (design 7.3): a
// repository of small JSON files that list, per namespace and name, the
// versions of modules and stacks: where the source of each version is (the
// repository, tag and commit of its author), its tree digest and its
// signatures. The code of a module stays in the repository of its author.
//
//	modules/<first letter>/<namespace>/<name>.json   the versions of one module
//	stacks/<first letter>/<namespace>/<name>.json    the versions of one stack
//	namespaces/<namespace>.json                      the owner and the source repositories
//	keys/<namespace>/*.pub                           the signing keys of the namespace
//	advisories.json                                  warnings and blocked versions
package index

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/bacanoo-io/norca/rigger/version"
)

// Kind is what an entry lists: a module or a stack.
type Kind string

// The kinds of entries.
const (
	KindModule Kind = "module"
	KindStack  Kind = "stack"
)

// AdvisoriesFile is the path of the list of advisories and blocked
// versions.
const AdvisoriesFile = "advisories.json"

// The layouts of a source repository of a namespace.
const (
	// LayoutRegistry is a registry repository (design 7.4): modules in
	// modules/<name>, stacks in stacks/<name>, tags <name>/v<version>.
	LayoutRegistry = "registry"
	// LayoutModule is a repository of one module at its root, tags
	// v<version>; the name of the module is the last element of the
	// repository.
	LayoutModule = "module"
)

// The levels of an advisory.
const (
	// LevelWarning is shown by rig stack sync and plan and changes nothing
	// else.
	LevelWarning = "warning"
	// LevelBlocked keeps a version out: it is not listed again, and a lock
	// file that names it fails with the reason.
	LevelBlocked = "blocked"
)

// ErrInvalid is the error of a file of the index that breaks its format.
var ErrInvalid = errors.New("invalid index file")

// Entry is the file of one module or stack: its versions, oldest first.
type Entry struct {
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	Versions  []Version `json:"versions"`
}

// Version is one listed version. It is never changed or removed once
// listed (design 7.3).
type Version struct {
	Version string `json:"version"`
	// Repository, Path and Tag name the source: the git repository of the
	// author, the directory in it, the tag of the version.
	Repository string `json:"repository"`
	Path       string `json:"path"`
	Tag        string `json:"tag"`
	// Commit is the commit the tag pointed to when the version was listed.
	Commit string `json:"commit"`
	// Digest is the tree digest of the version (design 8.1).
	Digest     string      `json:"digest"`
	Signatures []Signature `json:"signatures"`
	Requires   *Requires   `json:"requires,omitempty"`
	// Icon is the SHA-256 digest of the icon file, if the version has one.
	Icon string `json:"icon,omitempty"`
	// Modules are the registry modules the lock file of a stack pins.
	Modules []LockedModule `json:"modules,omitempty"`
	// Listed is the day the version was listed, YYYY-MM-DD.
	Listed string `json:"listed"`
}

// Signature is a signature of the statement of the version (sign.Statement
// of the digest, the name and the version) by a key of the namespace.
type Signature struct {
	KeyID     string `json:"key_id"`
	Signature string `json:"signature"`
}

// Requires is what a version needs: of the cluster, and the modules a stack
// must install next to it.
type Requires struct {
	Norca    string           `json:"norca,omitempty"`
	Features []string         `json:"features,omitempty"`
	Modules  []RequiredModule `json:"modules,omitempty"`
}

// RequiredModule is a module that must be installed next to a version.
type RequiredModule struct {
	Name    string `json:"name"`
	Source  string `json:"source,omitempty"`
	Version string `json:"version,omitempty"`
}

// LockedModule is a module version the lock of a stack pins.
type LockedModule struct {
	Source  string `json:"source"`
	Version string `json:"version"`
	Digest  string `json:"digest"`
}

// Namespace is the file of a namespace: who owns it and which repositories
// its entries are generated from.
type Namespace struct {
	Name string `json:"name"`
	// Owner is the user or organization that owns the source repositories.
	Owner        string       `json:"owner"`
	Repositories []Repository `json:"repositories"`
}

// Repository is a source repository of a namespace.
type Repository struct {
	// URL is the git address: github.com/acme/rig-modules.
	URL    string `json:"url"`
	Layout string `json:"layout"`
}

// Advisories is the file of advisories and blocked versions.
type Advisories struct {
	Advisories []Advisory `json:"advisories"`
}

// Advisory is a warning on a version, or a block that keeps it out.
type Advisory struct {
	Kind      Kind   `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	Level     string `json:"level"`
	Reason    string `json:"reason"`
	// Decision is where the decision to block is recorded (the four cases
	// of design 7.3 need one).
	Decision string `json:"decision,omitempty"`
}

// For returns the advisories of a version.
func (a *Advisories) For(kind Kind, namespace, name, v string) []Advisory {
	if a == nil {
		return nil
	}

	var out []Advisory

	for _, adv := range a.Advisories {
		if adv.Kind == kind && adv.Namespace == namespace && adv.Name == name && adv.Version == v {
			out = append(out, adv)
		}
	}

	return out
}

// Blocked returns the block of a version, if it has one.
func (a *Advisories) Blocked(kind Kind, namespace, name, v string) (Advisory, bool) {
	for _, adv := range a.For(kind, namespace, name, v) {
		if adv.Level == LevelBlocked {
			return adv, true
		}
	}

	return Advisory{}, false
}

// Version returns the listed version v, nil for none.
func (e *Entry) Version(v string) *Version {
	for i := range e.Versions {
		if e.Versions[i].Version == v {
			return &e.Versions[i]
		}
	}

	return nil
}

// dir is the top directory of the entries of a kind.
func dir(kind Kind) string {
	if kind == KindStack {
		return "stacks"
	}

	return "modules"
}

// EntryPath is the path of the entry of a module or stack.
func EntryPath(kind Kind, namespace, name string) string {
	return path.Join(dir(kind), namespace[:1], namespace, name+".json")
}

// NamespacePath is the path of the file of a namespace.
func NamespacePath(namespace string) string {
	return path.Join("namespaces", namespace+".json")
}

// KeysDir is the directory of the public keys of a namespace.
func KeysDir(namespace string) string {
	return path.Join("keys", namespace)
}

// ValidName reports whether s is a namespace or a name: lower case letters,
// digits and dashes, starting with a letter or digit, at most 63.
func ValidName(s string) bool {
	return namePattern.MatchString(s)
}

var (
	namePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	commitPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	keyIDPattern  = digestPattern
	dayPattern    = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
)

// ReadEntry reads the entry of a module or stack. The error matches
// fs.ErrNotExist when it is not listed.
func ReadEntry(fsys fs.FS, kind Kind, namespace, name string) (*Entry, error) {
	if !ValidName(namespace) || !ValidName(name) {
		return nil, fmt.Errorf("%w: %s/%s is not a namespace and a name", ErrInvalid, namespace, name)
	}

	p := EntryPath(kind, namespace, name)

	data, err := fs.ReadFile(fsys, p)
	if err != nil {
		return nil, err
	}

	var e Entry
	if err := decode(data, &e); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}

	if err := e.Validate(kind); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}

	if e.Namespace != namespace || e.Name != name {
		return nil, fmt.Errorf("%s: %w: it lists %s/%s", p, ErrInvalid, e.Namespace, e.Name)
	}

	return &e, nil
}

// ReadNamespace reads the file of a namespace.
func ReadNamespace(fsys fs.FS, namespace string) (*Namespace, error) {
	p := NamespacePath(namespace)

	data, err := fs.ReadFile(fsys, p)
	if err != nil {
		return nil, err
	}

	var ns Namespace
	if err := decode(data, &ns); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}

	if err := ns.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}

	if ns.Name != namespace {
		return nil, fmt.Errorf("%s: %w: it names %q", p, ErrInvalid, ns.Name)
	}

	return &ns, nil
}

// ReadKeys reads the public keys of a namespace (PEM), in the order of their
// file names; none when it has none.
func ReadKeys(fsys fs.FS, namespace string) ([][]byte, error) {
	entries, err := fs.ReadDir(fsys, KeysDir(namespace))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	var keys [][]byte

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".pub") {
			continue
		}

		data, err := fs.ReadFile(fsys, path.Join(KeysDir(namespace), e.Name()))
		if err != nil {
			return nil, err
		}

		keys = append(keys, data)
	}

	return keys, nil
}

// ReadAdvisories reads the advisories; none when the file is missing.
func ReadAdvisories(fsys fs.FS) (*Advisories, error) {
	data, err := fs.ReadFile(fsys, AdvisoriesFile)
	if errors.Is(err, fs.ErrNotExist) {
		return &Advisories{}, nil
	}

	if err != nil {
		return nil, err
	}

	var a Advisories
	if err := decode(data, &a); err != nil {
		return nil, fmt.Errorf("%s: %w", AdvisoriesFile, err)
	}

	if err := a.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", AdvisoriesFile, err)
	}

	return &a, nil
}

// decode reads JSON that has only known fields.
func decode(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	return nil
}

// Marshal writes a file of the index: indented JSON with a final newline,
// so that a change shows as lines in a review; ">=" stays as it is.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer

	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")

	if err := enc.Encode(v); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// Validate checks the fields of an entry: names, versions in ascending
// order without repetition, sources, digests and signatures in their
// formats; a stack's versions may pin modules, a module's may not.
func (e *Entry) Validate(kind Kind) error {
	if !ValidName(e.Namespace) || !ValidName(e.Name) {
		return fmt.Errorf("%w: %q/%q is not a namespace and a name", ErrInvalid, e.Namespace, e.Name)
	}

	var prev version.Version

	for i, v := range e.Versions {
		parsed, err := version.Parse(v.Version)
		if err != nil {
			return fmt.Errorf("%w: version %q: %w", ErrInvalid, v.Version, err)
		}

		if i > 0 && version.Compare(parsed, prev) <= 0 {
			return fmt.Errorf("%w: version %s is not after %s", ErrInvalid, v.Version, e.Versions[i-1].Version)
		}

		prev = parsed

		if err := v.validate(kind); err != nil {
			return fmt.Errorf("%w: version %s: %w", ErrInvalid, v.Version, err)
		}
	}

	return nil
}

var errField = errors.New("a field breaks its format")

func (v *Version) validate(kind Kind) error {
	if err := v.validateSource(); err != nil {
		return err
	}

	switch {
	case len(v.Signatures) == 0:
		return fmt.Errorf("%w: a listed version is signed", errField)
	case v.Icon != "" && !digestPattern.MatchString(v.Icon):
		return fmt.Errorf("%w: icon %q", errField, v.Icon)
	case !dayPattern.MatchString(v.Listed):
		return fmt.Errorf("%w: listed %q", errField, v.Listed)
	case kind == KindModule && len(v.Modules) > 0:
		return fmt.Errorf("%w: a module pins no modules", errField)
	}

	return v.validateLists()
}

// validateLists checks the signatures and the pinned modules of a version.
func (v *Version) validateLists() error {
	for _, s := range v.Signatures {
		if !keyIDPattern.MatchString(s.KeyID) || s.Signature == "" {
			return fmt.Errorf("%w: signature of %q", errField, s.KeyID)
		}
	}

	for _, m := range v.Modules {
		if m.Source == "" || m.Version == "" || !digestPattern.MatchString(m.Digest) {
			return fmt.Errorf("%w: module %q", errField, m.Source)
		}
	}

	return nil
}

// validateSource checks the fields that name the source of a version.
func (v *Version) validateSource() error {
	switch {
	case v.Repository == "" || v.Tag == "":
		return fmt.Errorf("%w: repository and tag are required", errField)
	case v.Path == "" || path.IsAbs(v.Path) || path.Clean(v.Path) != v.Path || strings.HasPrefix(v.Path, ".."):
		return fmt.Errorf("%w: path %q", errField, v.Path)
	case !commitPattern.MatchString(v.Commit):
		return fmt.Errorf("%w: commit %q", errField, v.Commit)
	case !digestPattern.MatchString(v.Digest):
		return fmt.Errorf("%w: digest %q", errField, v.Digest)
	default:
		return nil
	}
}

// Validate checks the fields of a namespace.
func (ns *Namespace) Validate() error {
	if !ValidName(ns.Name) || ns.Owner == "" {
		return fmt.Errorf("%w: name %q and owner %q", ErrInvalid, ns.Name, ns.Owner)
	}

	for _, r := range ns.Repositories {
		if r.URL == "" || strings.Contains(r.URL, "://") || (r.Layout != LayoutRegistry && r.Layout != LayoutModule) {
			return fmt.Errorf("%w: repository %q of layout %q", ErrInvalid, r.URL, r.Layout)
		}
	}

	return nil
}

// Validate checks the advisories.
func (a *Advisories) Validate() error {
	for _, adv := range a.Advisories {
		switch {
		case adv.Kind != KindModule && adv.Kind != KindStack:
			return fmt.Errorf("%w: advisory of kind %q", ErrInvalid, adv.Kind)
		case !ValidName(adv.Namespace) || !ValidName(adv.Name):
			return fmt.Errorf("%w: advisory of %q/%q", ErrInvalid, adv.Namespace, adv.Name)
		case adv.Level != LevelWarning && adv.Level != LevelBlocked:
			return fmt.Errorf("%w: advisory of level %q", ErrInvalid, adv.Level)
		case adv.Reason == "":
			return fmt.Errorf("%w: advisory of %s/%s %s without a reason", ErrInvalid, adv.Namespace, adv.Name, adv.Version)
		case adv.Level == LevelBlocked && adv.Decision == "":
			return fmt.Errorf("%w: block of %s/%s %s without a recorded decision", ErrInvalid, adv.Namespace, adv.Name, adv.Version)
		}

		if _, err := version.Parse(adv.Version); err != nil {
			return fmt.Errorf("%w: advisory of version %q", ErrInvalid, adv.Version)
		}
	}

	return nil
}

// ErrChanged is the error of a change of a listed version.
var ErrChanged = errors.New("a listed version is never changed or removed")

// CheckAppendOnly checks that next keeps every version of prev as it was
// (design 7.3); prev nil is a new entry.
func CheckAppendOnly(prev, next *Entry) error {
	if prev == nil {
		return nil
	}

	for _, old := range prev.Versions {
		v := next.Version(old.Version)
		if v == nil {
			return fmt.Errorf("%s/%s %s: %w (removed)", prev.Namespace, prev.Name, old.Version, ErrChanged)
		}

		a, _ := json.Marshal(old)
		b, _ := json.Marshal(v)

		if !bytes.Equal(a, b) {
			return fmt.Errorf("%s/%s %s: %w (changed)", prev.Namespace, prev.Name, old.Version, ErrChanged)
		}
	}

	return nil
}

// CheckAdvisoriesAppendOnly checks that next keeps every advisory of prev:
// an advisory, like a version, is added and never taken back.
func CheckAdvisoriesAppendOnly(prev, next *Advisories) error {
	for _, old := range prev.Advisories {
		if !slices.Contains(next.Advisories, old) {
			return fmt.Errorf("advisory of %s/%s %s: %w", old.Namespace, old.Name, old.Version, ErrChanged)
		}
	}

	return nil
}
