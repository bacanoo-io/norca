// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"sort"
	"strconv"

	"github.com/hashicorp/nomad/helper/flatmap"
)

// scalingTypedFields are the scalar fields of the typed scaling policy. They
// are left out of the generic primitive diff of the policy so that policies
// without a typed policy keep their diff unchanged.
var scalingTypedFields = []string{
	"EvaluationInterval", "Cooldown", "CooldownOnScaleUp", "OnCheckError", "ScaleDuringDeployment",
}

// scalingTypedDiff returns the field diffs of the scalar typed policy fields
// and the object diffs of the typed checks. Unset fields are not shown.
func scalingTypedDiff(old, new *ScalingPolicy, contextual bool) ([]*FieldDiff, []*ObjectDiff) {
	flat := func(p *ScalingPolicy) map[string]string {
		m := map[string]string{}
		if p.EvaluationInterval != 0 {
			m["EvaluationInterval"] = strconv.FormatInt(int64(p.EvaluationInterval), 10)
		}
		if p.Cooldown != 0 {
			m["Cooldown"] = strconv.FormatInt(int64(p.Cooldown), 10)
		}
		if p.CooldownOnScaleUp != 0 {
			m["CooldownOnScaleUp"] = strconv.FormatInt(int64(p.CooldownOnScaleUp), 10)
		}
		if p.OnCheckError != "" {
			m["OnCheckError"] = p.OnCheckError
		}
		if p.ScaleDuringDeployment != "" {
			m["ScaleDuringDeployment"] = p.ScaleDuringDeployment
		}
		return m
	}
	fields := fieldDiffs(flat(old), flat(new), contextual)

	byName := func(checks []*ScalingCheck) map[string]*ScalingCheck {
		m := make(map[string]*ScalingCheck, len(checks))
		for _, c := range checks {
			if c != nil {
				m[c.Name] = c
			}
		}
		return m
	}
	oldChecks, newChecks := byName(old.Checks), byName(new.Checks)

	names := make([]string, 0, len(oldChecks)+len(newChecks))
	for n := range oldChecks {
		names = append(names, n)
	}
	for n := range newChecks {
		if _, ok := oldChecks[n]; !ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)

	var objects []*ObjectDiff
	for _, n := range names {
		if d := scalingCheckDiff(oldChecks[n], newChecks[n], contextual); d != nil {
			objects = append(objects, d)
		}
	}
	return fields, objects
}

// scalingCheckDiff returns the diff of two typed scaling checks.
func scalingCheckDiff(old, new *ScalingCheck, contextual bool) *ObjectDiff {
	diff := &ObjectDiff{Type: DiffTypeNone, Name: "Check"}
	var oldFlat, newFlat map[string]string

	switch {
	case old.Equal(new):
		if !contextual {
			return nil
		}
		oldFlat = flatmap.Flatten(old, nil, false)
		newFlat = oldFlat
	case old == nil:
		diff.Type = DiffTypeAdded
		newFlat = flatmap.Flatten(new, nil, false)
	case new == nil:
		diff.Type = DiffTypeDeleted
		oldFlat = flatmap.Flatten(old, nil, false)
	default:
		diff.Type = DiffTypeEdited
		oldFlat = flatmap.Flatten(old, nil, false)
		newFlat = flatmap.Flatten(new, nil, false)
	}

	diff.Fields = fieldDiffs(oldFlat, newFlat, contextual)

	// Always show the name, which identifies the check.
	hasName := false
	for _, f := range diff.Fields {
		hasName = hasName || f.Name == "Name"
	}
	if !hasName {
		name := old.Name
		if new != nil {
			name = new.Name
		}
		diff.Fields = append(diff.Fields, &FieldDiff{Type: DiffTypeNone, Name: "Name", Old: name, New: name})
	}

	sort.Sort(FieldDiffs(diff.Fields))
	return diff
}
