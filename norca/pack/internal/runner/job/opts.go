// Copyright IBM Corp. 2021, 2025
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package job

import (
	"github.com/bacanoo-io/norca/norca/api"
)

func (r *Runner) newWriteOptsFromJob(job ParsedTemplate) *api.WriteOptions {
	opts := &api.WriteOptions{}
	if job.HasRegion() {
		opts.Region = *job.Job().Region
	}
	if job.HasNamespace() {
		opts.Namespace = *job.Job().Namespace
	}
	return opts
}

func (r *Runner) newWriteOptsFromClientJob(job *api.Job) *api.WriteOptions {
	opts := &api.WriteOptions{}
	if job.Region != nil {
		opts.Region = *job.Region
	}
	if job.Namespace != nil {
		opts.Namespace = *job.Namespace
	}
	return opts
}
