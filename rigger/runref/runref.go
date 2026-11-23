// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package runref holds the references a module makes to values that only
// exist on a node at run time (design 4.2): an environment variable of the
// task, a node attribute, an item of a secret or of a Variable, the
// addresses of a service.
//
// How such a reference is written depends on where its value lands. In an
// attribute of a job the client interpolates ${NORCA_ALLOC_DIR}; in the data
// of a task template the template reads {{ env "NORCA_ALLOC_DIR" }}; inside a
// string of an encoded configuration the value must also be escaped for that
// configuration. The engine does not know where a value lands: the job
// decoder of the host does, and an encoder knows how it quotes. So the
// functions of a module return a placeholder, a Part written into the text,
// and whoever knows the place resolves it with Replace.
package runref

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The delimiters of a placeholder: two characters of the private use area of
// Unicode, which no configuration writes.
const (
	delimOpen  = '\uE000'
	delimClose = '\uE001'
)

// Kind is what a part refers to.
type Kind string

// The kinds of parts.
const (
	// KindEnv is an environment variable of the task or a node attribute:
	// runtime_env(), runtime(), and a runtime reference written in a job.
	KindEnv Kind = "env"
	// KindSecret is an item of a secret of the built-in secret store.
	KindSecret Kind = "secret"
	// KindVariable is an item of a Norca Variable.
	KindVariable Kind = "variable"
	// KindService is one address of a service of the native catalog.
	KindService Kind = "service"
	// KindServices is the list of the addresses of a service.
	KindServices Kind = "services"
)

// Quote is how the value of a part is quoted where it lands.
type Quote string

// The quotes of a part.
const (
	// QuoteAuto lets the place decide: none in a task template, the
	// quoting of the configuration in an encoder.
	QuoteAuto Quote = ""
	// QuoteNone inserts the value as it is.
	QuoteNone Quote = "none"
	// QuoteConf is a double-quoted string, with \ and " escaped.
	QuoteConf Quote = "conf"
	// QuoteConfEscape escapes \ and " without quotes: a value inside a
	// double-quoted string.
	QuoteConfEscape Quote = "conf-escape"
	// QuoteSQL is a single-quoted string of SQL, with ' doubled.
	QuoteSQL Quote = "sql"
)

// Part is a reference to a value of run time.
type Part struct {
	Kind Kind `json:"k"`
	// Name is the variable or attribute of KindEnv, the path of a secret or
	// a Variable, the name of a service.
	Name string `json:"n"`
	// Item is the item of a secret or a Variable.
	Item  string `json:"i,omitempty"`
	Quote Quote  `json:"q,omitempty"`
	// Trim trims the white space around the value of a secret or a Variable.
	Trim bool `json:"t,omitempty"`
	// Scheme is written before each address of a service: scheme://.
	Scheme string `json:"s,omitempty"`
	// NoPort leaves the port out of the address of a service.
	NoPort bool `json:"p,omitempty"`
	// ExcludeSelf leaves the own allocation out of the addresses.
	ExcludeSelf bool `json:"x,omitempty"`
	// Lead is written before the first address, Sep between two: an
	// encoder that writes the addresses into a list sets them (", " after
	// the known items of the list). Without Sep the addresses are
	// separated by commas.
	Lead string `json:"l,omitempty"`
	Sep  string `json:"e,omitempty"`
}

// String returns the placeholder of the part.
func (p Part) String() string {
	data, err := json.Marshal(p)
	if err != nil {
		// A Part holds strings and bools only.
		panic(err)
	}

	return string(delimOpen) + base64.RawURLEncoding.EncodeToString(data) + string(delimClose)
}

// Contains reports whether s holds a placeholder.
func Contains(s string) bool {
	return strings.ContainsRune(s, delimOpen)
}

// ErrInvalid is the error of a malformed placeholder.
var ErrInvalid = errors.New("invalid runtime reference")

// Segment is a piece of a text: literal text, or a part.
type Segment struct {
	Text string
	Part *Part
}

// Split splits s into its literal text and its parts.
func Split(s string) ([]Segment, error) {
	var out []Segment

	for s != "" {
		i := strings.IndexRune(s, delimOpen)
		if i < 0 {
			out = append(out, Segment{Text: s})

			break
		}

		if i > 0 {
			out = append(out, Segment{Text: s[:i]})
		}

		s = s[i+len(string(delimOpen)):]

		j := strings.IndexRune(s, delimClose)
		if j < 0 {
			return nil, ErrInvalid
		}

		data, err := base64.RawURLEncoding.DecodeString(s[:j])
		if err != nil {
			return nil, ErrInvalid
		}

		var p Part
		if err := json.Unmarshal(data, &p); err != nil {
			return nil, ErrInvalid
		}

		out = append(out, Segment{Part: &p})
		s = s[j+len(string(delimClose)):]
	}

	return out, nil
}

// Replace replaces every part of s with what fn returns for it.
func Replace(s string, fn func(Part) (string, error)) (string, error) {
	if !Contains(s) {
		return s, nil
	}

	segs, err := Split(s)
	if err != nil {
		return "", err
	}

	var b strings.Builder

	for _, seg := range segs {
		if seg.Part == nil {
			b.WriteString(seg.Text)

			continue
		}

		text, err := fn(*seg.Part)
		if err != nil {
			return "", err
		}

		b.WriteString(text)
	}

	return b.String(), nil
}

// Place is where a value lands.
type Place int

// The places of a value.
const (
	// PlaceJob is an attribute of a job, which the client interpolates, and
	// every other value outside the data of a task template.
	PlaceJob Place = iota
	// PlaceTemplate is the data of a task template.
	PlaceTemplate
)

// Resolve returns the text of the part where it lands, in the quotes of its
// Quote: an encoder that found the part to be the whole value of a string
// wrote no quotes of its own and set QuoteConf, for every kind and in both
// places.
//
// Outside a task template the client writes the value in place of ${...}
// and has nothing to escape it with. So there the value is quoted, never
// escaped: one with a \ or a quote of its own is not what the configuration
// it lands in reads.
func (p Part) Resolve(place Place) (string, error) {
	if place == PlaceJob {
		if p.Kind != KindEnv {
			return "", fmt.Errorf("%s: %w", p.function(), ErrTemplateOnly)
		}

		return p.quoted("${" + p.Name + "}"), nil
	}

	switch p.Kind {
	case KindEnv:
		return p.quoted(fmt.Sprintf(`{{ env %s%s }}`, strconv.Quote(p.Name), p.filters())), nil
	case KindSecret:
		return p.quoted(fmt.Sprintf(`{{ with index (norcaSecret %s) %s }}{{ .%s }}{{ else }}{{ sprig_fail %s }}{{ end }}`,
			strconv.Quote(p.Name), strconv.Quote(p.Item), p.filters(),
			strconv.Quote(fmt.Sprintf("the secret %s has no item %s", p.Name, p.Item)))), nil
	case KindVariable:
		return p.quoted(fmt.Sprintf(`{{ with nomadVar %s }}{{ with index . %s }}{{ .Value%s }}{{ else }}{{ sprig_fail %s }}{{ end }}{{ end }}`,
			strconv.Quote(p.Name), strconv.Quote(p.Item), p.filters(),
			strconv.Quote(fmt.Sprintf("the Variable %s has no item %s", p.Name, p.Item)))), nil
	case KindService:
		return p.quoted(fmt.Sprintf(`{{ with nomadService %s }}{{ with index . 0 }}%s{{ end }}{{ end }}`,
			strconv.Quote(p.Name), p.address())), nil
	case KindServices:
		return p.services(), nil
	default:
		return "", fmt.Errorf("%w: kind %q", ErrInvalid, p.Kind)
	}
}

// ErrTemplateOnly is the error of a part that only a task template can
// resolve, found elsewhere.
var ErrTemplateOnly = errors.New("can be used only in the data of a task template")

// function names the function of the module that writes the part.
func (p Part) function() string {
	switch p.Kind {
	case KindEnv:
		return "runtime_env"
	case KindSecret:
		return "secret_ref"
	case KindVariable:
		return "variable_ref"
	case KindService:
		return "service_address"
	case KindServices:
		return "service_addresses"
	default:
		return string(p.Kind)
	}
}

// filters returns the template pipeline that trims and escapes the value.
func (p Part) filters() string {
	var f string
	if p.Trim {
		f += " | trimSpace"
	}

	switch p.Quote {
	case QuoteConf, QuoteConfEscape:
		f += ` | replaceAll "\\" "\\\\" | replaceAll "\"" "\\\""`
	case QuoteSQL:
		f += ` | replaceAll "'" "''"`
	case QuoteAuto, QuoteNone:
	}

	return f
}

// quoted puts the quotes of p.Quote around a template action, or around
// the ${...} of the client.
func (p Part) quoted(action string) string {
	switch p.Quote {
	case QuoteConf:
		return `"` + action + `"`
	case QuoteSQL:
		return `'` + action + `'`
	case QuoteAuto, QuoteNone, QuoteConfEscape:
	}

	return action
}

// address is the address of a service registration, the dot of a template.
func (p Part) address() string {
	a := p.Scheme
	if a != "" {
		a += "://"
	}

	a += "{{ .Address }}"
	if !p.NoPort {
		a += ":{{ .Port }}"
	}

	return a
}

// services is the list of the addresses of a service: Lead before the
// first, Sep (a comma by default) between two, each double-quoted with
// QuoteConf, as the items of a list of an encoded configuration.
func (p Part) services() string {
	sep, prefix, suffix := ",", "", ""
	if p.Sep != "" {
		sep = p.Sep
	}

	if p.Quote == QuoteConf {
		prefix, suffix = `"`, `"`
	}

	var b strings.Builder

	b.WriteString(`{{ $first := true }}{{ range nomadService `)
	b.WriteString(strconv.Quote(p.Name))
	b.WriteString(` }}`)

	if p.ExcludeSelf {
		b.WriteString(`{{ if ne .AllocID (env "NORCA_ALLOC_ID") }}`)
	}

	b.WriteString(`{{ if $first }}` + p.Lead + `{{ else }}` + sep + `{{ end }}{{ $first = false }}`)
	b.WriteString(prefix + p.address() + suffix)

	if p.ExcludeSelf {
		b.WriteString(`{{ end }}`)
	}

	b.WriteString(`{{ end }}`)

	return b.String()
}
