// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"fmt"
	"os"
	"strings"
)

// override records a reviewed license decision for a component whose
// license files cannot be classified automatically, or that is offered under
// a choice of licenses. Every entry needs a reason that a reviewer can
// verify against the component's own files.
type override struct {
	// Kind is "go" (module path; for a replaced module, the path of the
	// replacement) or "npm" (package name).
	Kind string
	Name string
	// Version, if set, limits the override to this version, so that an
	// update of the component is reviewed again.
	Version string
	// License is the SPDX expression under which the component is used.
	License string
	// Reason explains the decision.
	Reason string
	// Excerpt, if set, cuts the license files at the first line that
	// contains it; for license files that go on to describe software that
	// is not part of the module (see Reason).
	Excerpt string
	// Pending marks a decision that awaits legal review: the notices are
	// generated with it, but `-strict` (used by the release workflow) fails.
	Pending bool
	// Exception marks a decided exception to the allow-list, documented in
	// docs/licensing.md: License may be a license that is not allowed.
	Exception bool

	used bool
}

var overrides = []override{
	// Legacy UI exceptions (decision of 2026-10-25): accepted for the old
	// Ember UI only, which is served at /ui/legacy for one release and then
	// removed together with these entries. Not valid for the new UI. No
	// version pin, so that the Ember upgrade does not reopen the decision.
	{
		Kind:      "npm",
		Name:      "robust-predicates",
		License:   "Unlicense",
		Exception: true,
		Reason: "Legacy UI exception. The Unlicense (public domain dedication with a permissive fallback, OSI approved) is not on the " +
			"allow-list; the package is part of the legacy UI bundle through d3 > d3-delaunay > delaunator.",
	},
	{
		Kind:      "npm",
		Name:      "caniuse-lite",
		License:   "CC-BY-4.0",
		Exception: true,
		Reason: "Legacy UI exception. Browser support data used by browserslist while Babel builds the legacy UI " +
			"(@hashicorp/design-system-components > ember-cli-babel); a build-time dependency, not part of the UI bundle, listed " +
			"because the addon declares ember-cli-babel as a production dependency.",
	},
	{
		Kind:      "npm",
		Name:      "jsonify",
		License:   "LicenseRef-Public-Domain",
		Exception: true,
		Reason: "Legacy UI exception. Declared \"Public Domain\" in package.json, no license file. A build-time dependency " +
			"(@ember/test-waiters > calculate-cache-key-for-tree > json-stable-stringify), not part of the UI bundle.",
	},

	// Reviewed decisions.
	{
		Kind:    "go",
		Name:    "github.com/docker/go-metrics",
		Version: "v0.0.1",
		License: "Apache-2.0",
		Reason:  "The code is under the Apache License 2.0 (LICENSE); LICENSE.docs (CC-BY-SA-4.0) covers the documentation of the repository, which is not linked into the binaries.",
	},
	{
		Kind:    "go",
		Name:    "github.com/opencontainers/go-digest",
		Version: "v1.0.0",
		License: "Apache-2.0",
		Reason:  "The code is under the Apache License 2.0 (LICENSE); LICENSE.docs (CC-BY-SA-4.0) covers the documentation of the repository, which is not linked into the binaries.",
	},
	{
		Kind:    "npm",
		Name:    "dialog-polyfill",
		Version: "0.5.6",
		License: "BSD-3-Clause",
		Reason:  "package.json declares the non-SPDX value \"BSD\"; the LICENSE file is the BSD 3-clause text (The Chromium Authors).",
	},
	{
		Kind:    "go",
		Name:    "github.com/hashicorp/vic",
		Version: "v1.5.1-0.20190403131502-bbfe86ec9443",
		License: "Apache-2.0",
		Excerpt: "vSphere Integrated Containers Engine v1.5.2 GA includes a number of",
		Reason: "The LICENSE file is VMware's open source license disclosure for the vSphere Integrated Containers appliance: it states " +
			"that the product is licensed under the Apache License 2.0 and then lists the third-party components of the appliance " +
			"(operating system packages under GPL and other licenses), which are not part of this Go module. The only package linked, " +
			"pkg/vsphere/tags (via go-discover's vSphere provider), carries VMware's Apache 2.0 headers. Reproduced below: the Apache 2.0 part of the LICENSE.",
	},
	{
		Kind:    "go",
		Name:    "github.com/tj/go-spin",
		Version: "v1.1.0",
		License: "MIT",
		Reason:  "The module contains no license file; its Readme.md states \"License: MIT\" (author: TJ Holowaychuk). Linked through github.com/mitchellh/go-glint.",
	},
	{
		Kind:    "go",
		Name:    "kernel.org/pub/linux/libs/security/libcap/psx",
		License: "BSD-3-Clause",
		Reason:  "Dual-licensed: the License file offers \"one or the other\" of BSD 3-clause and GPL v2.0; Norca uses it under BSD-3-Clause. Linked through github.com/shoenig/go-landlock (Linux only).",
	},
}

func findOverride(kind, name, version string) *override {
	for i := range overrides {
		o := &overrides[i]
		if o.Kind == kind && o.Name == name && (o.Version == "" || o.Version == version) {
			o.used = true
			return o
		}
	}
	return nil
}

// checkOverrides reports overrides that matched no component (stale after
// an update; only if warnUnused) and fails, if strict, on pending overrides
// that were applied.
func checkOverrides(strict, warnUnused bool, kinds ...string) error {
	var problems []string
	for _, o := range overrides {
		relevant := false
		for _, k := range kinds {
			relevant = relevant || o.Kind == k
		}
		if !relevant {
			continue
		}
		id := fmt.Sprintf("%s %s %s", o.Kind, o.Name, o.Version)
		switch {
		case !o.used:
			if !warnUnused {
				continue
			}
			fmt.Fprintf(os.Stderr, "warning: override for %s matches no component; remove it from overrides.go\n", strings.TrimSpace(id))
		case o.Pending && strict:
			problems = append(problems, strings.TrimSpace(id)+": "+o.Reason)
		case o.Pending:
			fmt.Fprintf(os.Stderr, "warning: %s is shipped under a decision pending legal review (see overrides.go)\n", strings.TrimSpace(id))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("decisions pending legal review (-strict):\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

// applyExcerpt cuts the license texts at the override's excerpt marker.
func applyExcerpt(o *override, files []textFile) {
	if o == nil || o.Excerpt == "" {
		return
	}
	for i := range files {
		if j := strings.Index(files[i].Text, o.Excerpt); j >= 0 {
			cut := strings.LastIndex(files[i].Text[:j], "\n")
			if cut < 0 {
				cut = 0
			}
			files[i].Text = strings.TrimRight(files[i].Text[:cut], "\n=") + "\n\n[...] (excerpt; see the license decision above)"
		}
	}
}
