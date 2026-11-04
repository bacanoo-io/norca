// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package resourcespec

import (
	"errors"
	"fmt"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/helper/externalspec"
)

// ParseExternalService parses the file of an external service of a pack
// (the format of "norca service apply"): exactly one external_service block.
// An empty namespace is the namespace of the release.
func ParseExternalService(name string, content []byte) (*api.ExternalServiceItem, error) {
	items, err := externalspec.Parse(name, content, false)
	if err != nil {
		var pe *externalspec.Error
		if errors.As(err, &pe) && len(pe.Problems) > 0 {
			return nil, fmt.Errorf("%s", pe.Error())
		}
		return nil, err
	}
	if len(items) != 1 {
		return nil, fmt.Errorf("a file holds one external_service block, it has %d", len(items))
	}
	svc := items[0].Service
	svc.Declared = false
	return &api.ExternalServiceItem{Namespace: items[0].Namespace, Service: svc}, nil
}
