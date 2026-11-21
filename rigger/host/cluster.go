// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package host

import "context"

// ClusterInfo holds the facts of the cluster that an evaluation needs: the
// rigger.region fact and the check of the requires block of a module
// (design 4.2, 4.4).
type ClusterInfo struct {
	// Version is the version of Norca, in semver.
	Version string `json:"version"`
	// Region is the region the host installs into.
	Region string `json:"region"`
	// Features are the features the cluster announces, for example
	// "releases".
	Features []string `json:"features"`
}

// Cluster reports the facts of the cluster.
//
// Embedded in a server, the facts come from the leader, so that a lagging
// state can't change the result of an evaluation (design 12.7).
type Cluster interface {
	// Info returns the facts of the cluster.
	Info(ctx context.Context) (ClusterInfo, error)
	// Namespaces returns the names of the namespaces of the region.
	Namespaces(ctx context.Context) ([]string, error)
}
