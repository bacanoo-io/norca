// Copyright IBM Corp. 2021, 2025
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package parser

import (
	"github.com/bacanoo-io/norca/norca/pack/internal/pkg/variable/parser/config"
	"github.com/hashicorp/hcl/v2"
)

type Parser interface {
	Parse() (*ParsedVariables, hcl.Diagnostics)
}

func NewParser(cfg *config.ParserConfig) (Parser, error) {
	if cfg.Version == config.V1 {
		return NewParserV1(cfg)
	}
	return NewParserV2(cfg)
}
