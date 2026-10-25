// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package supplychain

import (
	"context"
	"os"
	"strings"

	"github.com/hashicorp/nomad/pack/internal/pkg/oci"
	"github.com/hashicorp/nomad/pack/internal/pkg/packconfig"
)

// MatchRegistry returns the configured registry whose URL contains loc, i.e.
// has the same host and a repository path that is a prefix of loc's. The
// longest match wins. It returns nil if no registry matches.
func MatchRegistry(conf *packconfig.Config, loc oci.Location) *packconfig.Registry {
	var best *packconfig.Registry
	bestLen := -1
	for _, r := range conf.Registries {
		rl := r.Location()
		if rl.Host != loc.Host {
			continue
		}
		if rl.Repository != "" && loc.Repository != rl.Repository && !strings.HasPrefix(loc.Repository, rl.Repository+"/") {
			continue
		}
		if len(rl.Repository) > bestLen {
			best, bestLen = r, len(rl.Repository)
		}
	}
	return best
}

// Endpoint is an OCI repository together with the connection options and the
// trust policy that apply to it.
type Endpoint struct {
	// RegistryName is the name of the configured registry, or "".
	RegistryName string
	Location     oci.Location
	Options      *oci.Options
	Policy       packconfig.Policy
}

// EndpointFor returns the endpoint of an OCI location: the settings of the
// configured registry that contains it, or defaults (HTTPS, credentials from
// the environment or docker configuration, default trust policy).
func EndpointFor(conf *packconfig.Config, loc oci.Location) *Endpoint {
	ep := &Endpoint{Location: loc, Options: &oci.Options{}, Policy: conf.TrustPolicy("")}
	if r := MatchRegistry(conf, loc); r != nil {
		ep.RegistryName = r.Name
		ep.Options = conf.Options(r)
		ep.Policy = conf.TrustPolicy(r.Name)
	}
	return ep
}

// Unpack downloads the pack at the endpoint's location with the given
// version (empty or "latest" for the newest), verifies it against the
// endpoint's trust policy and unpacks it into dest, replacing dest. It is used
// to vendor dependencies, which carry no origin record: the lock file records
// their digests instead.
func Unpack(ctx context.Context, ep *Endpoint, ref, dest string) (*Result, error) {
	repo, err := ep.Options.Repository(ep.Location)
	if err != nil {
		return nil, err
	}
	tag := ep.Location.Reference
	if tag == "" {
		if tag, err = ResolveTag(ctx, repo, ref); err != nil {
			return nil, err
		}
	}
	a, err := oci.Fetch(ctx, repo, tag)
	if err != nil {
		return nil, err
	}
	sigs, err := oci.FetchSignatures(ctx, repo, a.Digest())
	if err != nil {
		return nil, err
	}
	sigRes, err := ApplyPolicy(ep.Policy, a.Digest(), sigs)
	if err != nil {
		return nil, err
	}
	if err := os.RemoveAll(dest); err != nil {
		return nil, err
	}
	if err := a.Extract(dest); err != nil {
		return nil, err
	}
	return &Result{
		Registry:  ep.RegistryName,
		Source:    ep.Location.WithReference(tag).String(),
		Digest:    a.Digest(),
		Signature: sigRes,
	}, nil
}
