// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norcaapi

import (
	"fmt"
	"strings"

	"github.com/bacanoo-io/norca/norca/api"
)

// maxDiffLines bounds the lines of a job diff in a plan; the rest is
// counted in one more line.
const maxDiffLines = 20

// The types of a diff of the job API.
const (
	diffAdded   = "Added"
	diffDeleted = "Deleted"
	diffEdited  = "Edited"
)

// hiddenObjects are the objects of a job whose values a plan does not
// show: environment variables and templates often carry secrets, and a
// plan is shown in pull requests and logs. Their changes are listed
// without the values.
var hiddenObjects = map[string]bool{"Env": true, "Template": true}

// jobDiffLines is a job diff of the server's dry run as short lines, one per
// field that changes: "group web: Count: 1 -> 3". A job that is added or
// deleted as a whole is one line; nil, or a diff without changes, is none.
func jobDiffLines(d *api.JobDiff) []string {
	if d == nil {
		return nil
	}

	switch d.Type {
	case diffAdded:
		return []string{"job added"}
	case diffDeleted:
		return []string{"job deleted"}
	case diffEdited:
	default:
		return nil
	}

	var lines []string

	fieldLines(&lines, "", d.Fields, false)
	objectLines(&lines, "", d.Objects, false)

	for _, tg := range d.TaskGroups {
		path := "group " + tg.Name

		switch tg.Type {
		case diffAdded, diffDeleted:
			lines = append(lines, path+": "+strings.ToLower(tg.Type))

			continue
		}

		fieldLines(&lines, path, tg.Fields, false)
		objectLines(&lines, path, tg.Objects, false)

		for _, task := range tg.Tasks {
			taskPath := path + ", task " + task.Name

			switch task.Type {
			case diffAdded, diffDeleted:
				lines = append(lines, taskPath+": "+strings.ToLower(task.Type))

				continue
			}

			fieldLines(&lines, taskPath, task.Fields, false)
			objectLines(&lines, taskPath, task.Objects, false)
		}
	}

	if len(lines) > maxDiffLines {
		more := len(lines) - maxDiffLines
		lines = append(lines[:maxDiffLines], fmt.Sprintf("and %d more", more))
	}

	return lines
}

// objectLines adds the lines of the fields of objects below path; the values
// below a hidden object are left out.
func objectLines(lines *[]string, path string, objects []*api.ObjectDiff, hidden bool) {
	for _, o := range objects {
		if o == nil {
			continue
		}

		objPath := join(path, o.Name)
		hide := hidden || hiddenObjects[o.Name]

		fieldLines(lines, objPath, o.Fields, hide)
		objectLines(lines, objPath, o.Objects, hide)
	}
}

// fieldLines adds a line for each field of fields that changes.
func fieldLines(lines *[]string, path string, fields []*api.FieldDiff, hidden bool) {
	for _, f := range fields {
		if f == nil {
			continue
		}

		name := join(path, f.Name)

		switch {
		case f.Type != diffAdded && f.Type != diffDeleted && f.Type != diffEdited:
			continue
		case hidden:
			*lines = append(*lines, name+": "+strings.ToLower(f.Type)+" (value not shown)")
		case f.Type == diffAdded:
			*lines = append(*lines, fmt.Sprintf("%s: added %q", name, f.New))
		case f.Type == diffDeleted:
			*lines = append(*lines, fmt.Sprintf("%s: deleted %q", name, f.Old))
		default:
			*lines = append(*lines, fmt.Sprintf("%s: %q -> %q", name, f.Old, f.New))
		}
	}
}

// join is path and name as a path of a diff line.
func join(path, name string) string {
	if path == "" {
		return name
	}

	return path + ": " + name
}
