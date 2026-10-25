// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package cli

import (
	"fmt"
	"slices"
	"strconv"

	"github.com/hashicorp/nomad/api"

	"github.com/hashicorp/nomad/pack/terminal"
)

// Multi-region installs: run, upgrade and plan with --regions (or regions in
// metadata.hcl) send one request; the servers apply the release in every
// region and return the results per region. status, history and uninstall
// work on the releases of all regions of the install, which they find in
// the multiregion description of the release (or take from --regions).

// inRegion returns ` in region "<region>"` for messages, or "".
func inRegion(region string) string {
	if region == "" {
		return ""
	}
	return fmt.Sprintf(" in region %q", region)
}

// printRegionResults prints the results of a multi-region install or plan
// per region. It returns 1 if the operation failed in a region.
func (c *baseCommand) printRegionResults(name string, resp *api.ReleaseApplyResponse, plan bool) int {
	if len(resp.Regions) == 0 {
		// Servers without multi-region installs ignore the regions.
		printChanges(c.ui, resp.Changes)
		printWarnings(c.ui, resp.Warnings)
		c.ui.Warning(fmt.Sprintf("The servers do not support multi-region installs; release %q was applied in their region only", name))
		if !plan {
			c.ui.Success(fmt.Sprintf("Release %q deployed at revision %d", name, resp.Revision))
		}
		return 0
	}

	failed := 0
	tbl := terminal.NewTable("Region", "Result", "Revision", "Phase", "Error")
	for _, r := range resp.Regions {
		result := "deployed"
		if plan {
			result = "planned"
		}
		if r.Error != "" {
			result = "failed"
			failed++
		}
		phase := ""
		if r.Release != nil && r.Release.Status != nil {
			phase = r.Release.Status.Phase
		}
		rev := ""
		if r.Revision > 0 {
			rev = strconv.FormatUint(r.Revision, 10)
		}
		tbl.Rows = append(tbl.Rows, []string{r.Region, result, rev, phase, limit(r.Error, 120)})
	}
	for _, r := range resp.Regions {
		if len(r.Changes) == 0 {
			continue
		}
		c.ui.Output(fmt.Sprintf("Region %q:", r.Region))
		printChanges(c.ui, r.Changes)
		c.ui.Output("")
	}
	c.ui.Table(tbl)
	printWarnings(c.ui, resp.Warnings)

	switch {
	case failed > 0 && plan:
		c.ui.ErrorWithContext(fmt.Errorf("the plan failed in %d of %d regions", failed, len(resp.Regions)),
			"plan failed", "Release: "+name)
		return 1
	case failed > 0:
		c.ui.ErrorWithContext(fmt.Errorf("release %q failed in %d of %d regions; the releases of the other regions are kept",
			name, failed, len(resp.Regions)), "release failed", "Release: "+name)
		return 1
	case !plan:
		c.ui.Success(fmt.Sprintf("Release %q deployed in %d regions", name, len(resp.Regions)))
	}
	return 0
}

// releaseRegions returns the regions of the install of a release: the
// regions given, or those recorded in the release of the local region
// (nil for a release of one region). It also returns the global region.
func releaseRegions(client *api.Client, name string, given []string) (regions []string, global string) {
	var rel *api.Release
	if len(given) > 0 {
		for _, region := range given {
			if r, _, err := client.Releases().Info(name, &api.QueryOptions{Region: region}); err == nil {
				rel = r
				break
			}
		}
	} else if r, _, err := client.Releases().Info(name, nil); err == nil {
		rel = r
	}
	if rel != nil && rel.Spec != nil && rel.Spec.Multiregion != nil {
		global = rel.Spec.Multiregion.GlobalRegion
		if len(given) == 0 {
			given = rel.Spec.Multiregion.Regions
		}
	}
	return given, global
}

// regionRelease is the release of a region of an install, or the error of
// reading it.
type regionRelease struct {
	region  string
	release *api.Release
	err     error
}

// regionReleases reads the release in every region.
func regionReleases(client *api.Client, name string, regions []string) []*regionRelease {
	out := make([]*regionRelease, 0, len(regions))
	for _, region := range regions {
		rel, _, err := client.Releases().Info(name, &api.QueryOptions{Region: region})
		out = append(out, &regionRelease{region: region, release: rel, err: err})
	}
	return out
}

// printRegionReleases prints the state of the release in every region of
// its install.
func printRegionReleases(ui terminal.UI, global string, releases []*regionRelease) {
	tbl := terminal.NewTable("Region", "Phase", "Deployed", "Latest", "Ready", "Drifted", "Last error")
	for _, rr := range releases {
		region := rr.region
		if region == global {
			region += " (global)"
		}
		if rr.err != nil {
			tbl.Rows = append(tbl.Rows, []string{region, "-", "-", "-", "-", "-", limit(rr.err.Error(), 80)})
			continue
		}
		spec, status := rr.release.Spec, rr.release.Status
		if spec == nil {
			spec = &api.ReleaseSpec{}
		}
		if status == nil {
			status = &api.ReleaseStatus{}
		}
		lastErr := ""
		if op := status.LastOperation; op != nil {
			lastErr = limit(op.Error, 80)
		}
		tbl.Rows = append(tbl.Rows, []string{
			region, status.Phase,
			strconv.FormatUint(status.Revision, 10), strconv.FormatUint(spec.Revision, 10),
			conditionStatus(status, api.ReleaseConditionReady),
			conditionStatus(status, api.ReleaseConditionDrifted),
			lastErr,
		})
	}
	ui.Table(tbl)
}

// uninstallOrder returns the regions in the order their releases are
// uninstalled: the reverse of the install, so that the global region, whose
// release owns the global resources, comes last.
func uninstallOrder(regions []string, global string) []string {
	out := make([]string, 0, len(regions))
	for i := len(regions) - 1; i >= 0; i-- {
		if regions[i] != global {
			out = append(out, regions[i])
		}
	}
	if slices.Contains(regions, global) {
		out = append(out, global)
	}
	return out
}
