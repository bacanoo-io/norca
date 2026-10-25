// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"fmt"

	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/nomad/nomad/structs"
)

// jobMTLSHook validates jobs that use native mTLS: every server must run
// Norca, the certificate authority must be enabled and the namespace, job and
// group names must fit into a SPIFFE ID.
type jobMTLSHook struct {
	srv *Server
}

func (jobMTLSHook) Name() string {
	return "mtls"
}

func (h jobMTLSHook) Validate(job *structs.Job) ([]error, error) {
	if !job.UsesMTLS() {
		return nil, nil
	}
	if !ServersMeetMinimumNorcaVersion(h.srv.serf.Members(), h.srv.Region(), false) {
		return nil, fmt.Errorf("all servers must be running Norca %v or later to use mtls blocks", minNorcaFeaturesVersion)
	}
	if !h.srv.mtlsConfig().IsEnabled() {
		return nil, fmt.Errorf("mtls blocks require native mTLS, which is disabled in the server configuration")
	}

	var mErr multierror.Error
	var warnings []error
	if err := structs.ValidateSPIFFESegment("namespace", job.Namespace); err != nil {
		mErr.Errors = append(mErr.Errors, err)
	}
	if job.ParentID == "" {
		if err := structs.ValidateSPIFFESegment("job ID", job.ID); err != nil {
			mErr.Errors = append(mErr.Errors, err)
		}
	}
	for _, tg := range job.TaskGroups {
		services := tg.MTLSServices()
		if len(services) == 0 {
			continue
		}
		if err := structs.ValidateSPIFFESegment("group name", tg.Name); err != nil {
			mErr.Errors = append(mErr.Errors, err)
		}
		for _, service := range services {
			if !structs.IsValidMTLSServiceName(service.Name) {
				warnings = append(warnings, fmt.Errorf(
					"group %q: service %q is interpolated at runtime and is left out of the DNS names of the mTLS certificate, so intentions can't refer to it",
					tg.Name, service.Name))
			}
		}
	}
	return warnings, mErr.ErrorOrNil()
}
