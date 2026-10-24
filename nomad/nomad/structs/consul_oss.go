// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

//go:build !ent
// +build !ent

package structs

import (
	"errors"
	"fmt"
)

// ErrConsulNamespaceNotSupported is returned for a job that sets a Consul
// namespace. Services, Connect and templates always use the namespace of the
// agent configuration (GetNamespace returns ""), so the job field would be
// silently ignored.
var ErrConsulNamespaceNotSupported = errors.New("consul namespace in a job is not supported, set it in the consul block of the agent configuration")

func (c *Consul) GetNamespace() string {
	return ""
}

// validateConsulNamespaces returns an error if the job or one of its task
// groups sets a Consul namespace.
func (j *Job) validateConsulNamespaces() error {
	if j.ConsulNamespace != "" {
		return fmt.Errorf("%w: job consul_namespace %q", ErrConsulNamespaceNotSupported, j.ConsulNamespace)
	}
	for _, tg := range j.TaskGroups {
		if tg.Consul != nil && tg.Consul.Namespace != "" {
			return fmt.Errorf("%w: task group %q consul namespace %q", ErrConsulNamespaceNotSupported, tg.Name, tg.Consul.Namespace)
		}
	}
	return nil
}
