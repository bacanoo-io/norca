#!/usr/bin/env bash
# Copyright (c) HashiCorp, Inc.
# Modifications Copyright (c) 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0


nomad operator api "/v1/allocation/${NORCA_ALLOC_ID}" | jq '.NetworkStatus.Address | length'
