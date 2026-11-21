// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package norcaapi implements the ports of Rigger (package host) over Norca:
// its HTTP API and its job decoder (design 12.1, 12.2). It is public so
// that the deployer can link it.
//
// Phase 0 holds the job decoder; the adapters over the HTTP API follow.
package norcaapi
