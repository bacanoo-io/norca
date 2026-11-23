// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package version holds the versions of modules and stacks and the
// constraints on them (design 7.2): semantic versions, and constraints such
// as "~> 1.2" or ">= 1.0, < 2".
package version

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Version is a semantic version: major.minor.patch, an optional
// pre-release; build metadata is kept but takes no part in the order.
type Version struct {
	Major, Minor, Patch uint64
	Pre                 []string
	Build               string
}

// ErrInvalid is the error of a text that is not a version or a constraint.
var ErrInvalid = errors.New("invalid version")

// Parse parses a version of Semantic Versioning 2.0.0, with an optional
// leading v: three numbers, then an optional pre-release after a dash and
// optional build metadata after a plus, both identifiers of letters, digits
// and dashes separated by dots. No number, and no identifier of a
// pre-release that is a number, starts with a zero it does not need.
//
// Parse is the one check of a version that comes from outside (an index, a
// tag of a repository or of an OCI registry), so a text it takes holds
// nothing but those characters: no white space, no path, no control
// character.
func Parse(s string) (Version, error) {
	text := strings.TrimPrefix(s, "v")

	text, build, hasBuild := strings.Cut(text, "+")
	core, pre, hasPre := strings.Cut(text, "-")

	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("%w: %q", ErrInvalid, s)
	}

	nums := make([]uint64, 3)

	for i, p := range parts {
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil || padded(p) {
			return Version{}, fmt.Errorf("%w: %q", ErrInvalid, s)
		}

		nums[i] = n
	}

	v := Version{Major: nums[0], Minor: nums[1], Patch: nums[2]}

	if hasPre {
		v.Pre = strings.Split(pre, ".")

		for _, id := range v.Pre {
			if !isIdentifier(id) || (isNumber(id) && padded(id)) {
				return Version{}, fmt.Errorf("%w: %q", ErrInvalid, s)
			}
		}
	}

	if hasBuild {
		for id := range strings.SplitSeq(build, ".") {
			if !isIdentifier(id) {
				return Version{}, fmt.Errorf("%w: %q", ErrInvalid, s)
			}
		}

		v.Build = build
	}

	return v, nil
}

// isIdentifier reports whether s is an identifier of a pre-release or of
// build metadata: one or more ASCII letters, digits and dashes.
func isIdentifier(s string) bool {
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && c != '-' {
			return false
		}
	}

	return s != ""
}

// isNumber reports whether s is one or more ASCII digits.
func isNumber(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}

	return s != ""
}

// padded reports whether a number starts with a zero it does not need.
func padded(s string) bool {
	return len(s) > 1 && s[0] == '0'
}

// String writes the version without a leading v.
func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if len(v.Pre) > 0 {
		s += "-" + strings.Join(v.Pre, ".")
	}

	if v.Build != "" {
		s += "+" + v.Build
	}

	return s
}

// Compare orders two versions: -1, 0 or 1. A pre-release comes before its
// version; build metadata is ignored.
func Compare(a, b Version) int {
	for _, d := range [][2]uint64{{a.Major, b.Major}, {a.Minor, b.Minor}, {a.Patch, b.Patch}} {
		if d[0] != d[1] {
			if d[0] < d[1] {
				return -1
			}

			return 1
		}
	}

	switch {
	case len(a.Pre) == 0 && len(b.Pre) == 0:
		return 0
	case len(a.Pre) == 0:
		return 1
	case len(b.Pre) == 0:
		return -1
	}

	for i := range min(len(a.Pre), len(b.Pre)) {
		if c := comparePre(a.Pre[i], b.Pre[i]); c != 0 {
			return c
		}
	}

	switch {
	case len(a.Pre) < len(b.Pre):
		return -1
	case len(a.Pre) > len(b.Pre):
		return 1
	default:
		return 0
	}
}

// comparePre orders two identifiers of a pre-release: numbers by value and
// before words, words by their bytes.
func comparePre(a, b string) int {
	na, nb := isNumber(a), isNumber(b)

	switch {
	case na && nb:
		return compareNumbers(a, b)
	case na:
		return -1
	case nb:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

// compareNumbers orders two texts of digits by their value. A number of a
// pre-release has no limit of size, so the texts are not converted: without
// the zeros before them the longer is the larger, and two of one length are
// in the order of their bytes.
func compareNumbers(a, b string) int {
	a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")

	if len(a) != len(b) {
		return cmp.Compare(len(a), len(b))
	}

	return strings.Compare(a, b)
}

// Constraint is a set of conditions on a version, all of which must hold.
type Constraint struct {
	text  string
	conds []condition
	// pre is set when a condition asks for a pre-release: only then may a
	// pre-release meet the constraint. A "!=" names one to keep it out, and
	// asks for none.
	pre bool
}

// The operators whose rule for a pre-release is their own.
const (
	opNot  = "!="
	opLess = "<"
)

type condition struct {
	op string
	v  Version
	// parts is the number of parts written of a "~>" version: 1, 2 or 3.
	parts int
}

// ParseConstraint parses a constraint: conditions separated by commas, each
// an operator (=, !=, >, >=, <, <=, ~>) and a version; a version alone is
// "=". "~> 1.2" is >= 1.2.0 and < 2.0.0, "~> 1.2.3" >= 1.2.3 and < 1.3.0.
//
// A pre-release meets a constraint only when a condition other than "!="
// names a pre-release, and never when it is a pre-release of the version an
// upper bound keeps out: ">= 1.0.0-rc.1, < 2.0.0" takes 1.5.0-beta.1 and
// not 2.0.0-alpha.
func ParseConstraint(s string) (Constraint, error) {
	c := Constraint{text: strings.TrimSpace(s)}

	for part := range strings.SplitSeq(s, ",") {
		cond, err := parseCondition(strings.TrimSpace(part))
		if err != nil {
			return Constraint{}, fmt.Errorf("%w: constraint %q", err, s)
		}

		c.conds = append(c.conds, cond)
		c.pre = c.pre || (len(cond.v.Pre) > 0 && cond.op != opNot)
	}

	return c, nil
}

func parseCondition(s string) (condition, error) {
	op := "="

	for _, o := range []string{"~>", ">=", "<=", opNot, ">", opLess, "="} {
		if rest, ok := strings.CutPrefix(s, o); ok {
			op, s = o, strings.TrimSpace(rest)

			break
		}
	}

	// A "~>", ">=", "<" or "=" may leave out the minor or the patch.
	core, rest := s, ""
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		core, rest = s[:i], s[i:]
	}

	parts := len(strings.Split(core, "."))
	if parts < 1 || parts > 3 || (parts < 3 && op != "~>" && op != ">=" && op != opLess && op != "=") {
		return condition{}, ErrInvalid
	}

	text := core + strings.Repeat(".0", 3-parts) + rest

	v, err := Parse(text)
	if err != nil {
		return condition{}, err
	}

	return condition{op: op, v: v, parts: parts}, nil
}

// String is the constraint as written.
func (c Constraint) String() string { return c.text }

// Check reports whether v meets every condition.
func (c Constraint) Check(v Version) bool {
	if len(v.Pre) > 0 && !c.pre {
		return false
	}

	for _, cond := range c.conds {
		if !cond.check(v) {
			return false
		}
	}

	return true
}

func (c condition) check(v Version) bool {
	cmp := Compare(v, c.v)

	switch c.op {
	case "=":
		return cmp == 0
	case opNot:
		return cmp != 0
	case ">":
		return cmp > 0
	case ">=":
		return cmp >= 0
	case opLess:
		// A bound that keeps a version out keeps out the pre-releases of it:
		// in the order they are before the version, and they are of it. A
		// bound that names a pre-release is one in the order as it is.
		if len(c.v.Pre) == 0 {
			return Compare(Version{Major: v.Major, Minor: v.Minor, Patch: v.Patch}, c.v) < 0
		}

		return cmp < 0
	case "<=":
		return cmp <= 0
	default: // ~>
		if cmp < 0 {
			return false
		}

		upper := Version{Major: c.v.Major + 1}
		if c.parts == 3 {
			upper = Version{Major: c.v.Major, Minor: c.v.Minor + 1}
		}

		return Compare(Version{Major: v.Major, Minor: v.Minor, Patch: v.Patch}, upper) < 0
	}
}

// Newest returns the newest of versions that meets c.
func (c Constraint) Newest(versions []Version) (Version, bool) {
	var ok []Version

	for _, v := range versions {
		if c.Check(v) {
			ok = append(ok, v)
		}
	}

	if len(ok) == 0 {
		return Version{}, false
	}

	return slices.MaxFunc(ok, Compare), true
}
