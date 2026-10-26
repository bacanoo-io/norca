// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"reflect"
	"sort"

	"github.com/hashicorp/nomad/helper/flatmap"
)

// Diffs of the secret delivery blocks (Norca): secrets blocks of jobs and
// groups, secret_access bindings and the secret blocks of tasks.

// secretsConfigDiff diffs the secrets blocks of a job or group.
func secretsConfigDiff(old, new *SecretsConfig, contextual bool) *ObjectDiff {
	if old.Equal(new) {
		return nil
	}
	var o, n interface{}
	if old != nil {
		o = old
	}
	if new != nil {
		n = new
	}
	return primitiveObjectDiff(o, n, nil, "Secrets", contextual)
}

// secretAccessDiffs diffs the secret_access bindings of a group or task.
func secretAccessDiffs(old, new []*SecretAccess, contextual bool) []*ObjectDiff {
	if SecretAccessEqual(old, new) {
		return nil
	}
	return primitiveObjectSetDiff(interfaceSlice(old), interfaceSlice(new), nil, "SecretAccess", contextual)
}

// taskSecretDiffs diffs the secret blocks of a task, matched by name.
func taskSecretDiffs(old, new []*TaskSecret, contextual bool) []*ObjectDiff {
	oldByName := make(map[string]*TaskSecret, len(old))
	for _, s := range old {
		oldByName[s.Name] = s
	}
	newByName := make(map[string]*TaskSecret, len(new))
	for _, s := range new {
		newByName[s.Name] = s
	}

	var diffs []*ObjectDiff
	for name, o := range oldByName {
		if d := taskSecretDiff(o, newByName[name], contextual); d != nil {
			diffs = append(diffs, d)
		}
	}
	for name, n := range newByName {
		if _, ok := oldByName[name]; !ok {
			if d := taskSecretDiff(nil, n, contextual); d != nil {
				diffs = append(diffs, d)
			}
		}
	}
	sort.Sort(ObjectDiffs(diffs))
	return diffs
}

// taskSecretDiff diffs two secret blocks of the same name.
func taskSecretDiff(old, new *TaskSecret, contextual bool) *ObjectDiff {
	diff := &ObjectDiff{Type: DiffTypeNone, Name: "Secret"}
	var oldPrimitiveFlat, newPrimitiveFlat map[string]string

	if reflect.DeepEqual(old, new) {
		return nil
	} else if old == nil {
		old = &TaskSecret{}
		diff.Type = DiffTypeAdded
		newPrimitiveFlat = flatmap.Flatten(new, nil, true)
	} else if new == nil {
		new = &TaskSecret{}
		diff.Type = DiffTypeDeleted
		oldPrimitiveFlat = flatmap.Flatten(old, nil, true)
	} else {
		diff.Type = DiffTypeEdited
		oldPrimitiveFlat = flatmap.Flatten(old, nil, true)
		newPrimitiveFlat = flatmap.Flatten(new, nil, true)
	}

	diff.Fields = fieldDiffs(oldPrimitiveFlat, newPrimitiveFlat, contextual)

	var oldFile, newFile interface{}
	if old.File != nil {
		oldFile = old.File
	}
	if new.File != nil {
		newFile = new.File
	}
	if fDiff := primitiveObjectDiff(oldFile, newFile, nil, "File", contextual); fDiff != nil {
		diff.Objects = append(diff.Objects, fDiff)
	}

	// The Env items are primitive fields of the flattened secret
	// ("Env[DB_USER]").
	if csDiff := changeScriptDiff(old.ChangeScript, new.ChangeScript, contextual); csDiff != nil {
		diff.Objects = append(diff.Objects, csDiff)
	}
	return diff
}
