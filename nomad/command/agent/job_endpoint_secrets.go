// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"maps"
	"slices"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/nomad/structs"
)

// Conversions of the secret delivery blocks of jobs (Norca).

// apiSecretsConfigToStructs converts a secrets block. The namespace default
// is set by the servers, so a submitted one is dropped.
func apiSecretsConfigToStructs(in *api.SecretsConfig) *structs.SecretsConfig {
	if in == nil {
		return nil
	}
	return &structs.SecretsConfig{DefaultProvider: in.DefaultProvider}
}

func apiSecretAccessToStructs(in []*api.SecretAccess) []*structs.SecretAccess {
	if len(in) == 0 {
		return nil
	}
	out := make([]*structs.SecretAccess, 0, len(in))
	for _, a := range in {
		if a != nil {
			out = append(out, &structs.SecretAccess{Path: a.Path})
		}
	}
	return out
}

func apiTaskSecretsToStructs(in []*api.TaskSecret) []*structs.TaskSecret {
	if len(in) == 0 {
		return nil
	}
	out := make([]*structs.TaskSecret, 0, len(in))
	for _, s := range in {
		if s == nil {
			continue
		}
		ts := &structs.TaskSecret{
			Name:         s.Name,
			Provider:     s.Provider,
			Path:         s.Path,
			Version:      s.Version,
			Engine:       s.Engine,
			Role:         s.Role,
			Env:          maps.Clone(s.Env),
			ChangeMode:   s.ChangeMode,
			ChangeSignal: s.ChangeSignal,
			Optional:     s.Optional,
			Timeout:      s.Timeout,
		}
		if s.File != nil {
			ts.File = &structs.TaskSecretFile{
				Destination: s.File.Destination,
				Format:      s.File.Format,
				Template:    s.File.Template,
			}
		}
		if cs := s.ChangeScript; cs != nil {
			ts.ChangeScript = &structs.ChangeScript{Args: slices.Clone(cs.Args)}
			if cs.Command != nil {
				ts.ChangeScript.Command = *cs.Command
			}
			if cs.Timeout != nil {
				ts.ChangeScript.Timeout = *cs.Timeout
			}
			if cs.FailOnError != nil {
				ts.ChangeScript.FailOnError = *cs.FailOnError
			}
		}
		out = append(out, ts)
	}
	return out
}
