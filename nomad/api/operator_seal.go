// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import "net/url"

// Keyring seal types.
const (
	SealTypeAEAD   = "aead"
	SealTypeShares = "shares"
)

// SealInfo describes one seal of the region. It never contains a key.
type SealInfo struct {
	Type      string
	KeyID     string
	Threshold int
	Shares    int
}

// SealRewrap is a seal migration in progress.
type SealRewrap struct {
	ID        string
	To        []*SealInfo
	Done      []string
	StartTime int64
}

// SealConfig is the keyring seal configuration of the region. It is nil in
// a region that uses the implicit aead seal.
type SealConfig struct {
	Seals        []*SealInfo
	Rewrap       *SealRewrap
	LastRewrapID string
	CreateIndex  uint64
	ModifyIndex  uint64
}

// ServerSealStatus is the seal status of one server.
type ServerSealStatus struct {
	ID                    string
	Name                  string
	Address               string
	Leader                bool
	Sealed                bool
	Initialized           bool
	Seals                 []string
	ConfiguredSeal        string
	Threshold             int
	Shares                int
	Progress              int
	KeyIDs                []string
	SecretCoreInitialized bool
	SecretCoreSealed      bool
	SecretCoreActive      bool
	Error                 string
}

// SealStatusResponse is the seal status of the region.
type SealStatusResponse struct {
	Config  *SealConfig
	Servers []*ServerSealStatus
}

// SealStatus returns the seal status of every server of the region, or of
// one server if serverID is set.
func (op *Operator) SealStatus(serverID string, q *QueryOptions) (*SealStatusResponse, *QueryMeta, error) {
	path := "/v1/operator/seal"
	if serverID != "" {
		path += "?server_id=" + url.QueryEscape(serverID)
	}
	var resp SealStatusResponse
	qm, err := op.c.query(path, &resp, q)
	if err != nil {
		return nil, nil, err
	}
	return &resp, qm, nil
}

// UnsealRequest enters one unseal share on a server.
type UnsealRequest struct {
	// ServerID is the server name or ID; empty for the server of the agent
	// the client talks to.
	ServerID string `json:"-"`

	// Key is one unseal share, base64 encoded.
	Key string

	// Reset discards the shares entered so far.
	Reset bool
}

// UnsealResponse is the seal status of the server after the share.
type UnsealResponse struct {
	Status *ServerSealStatus
}

// Unseal enters one unseal share on a server.
func (op *Operator) Unseal(req *UnsealRequest, w *WriteOptions) (*ServerSealStatus, *WriteMeta, error) {
	path := "/v1/operator/unseal"
	if req.ServerID != "" {
		path += "?server_id=" + url.QueryEscape(req.ServerID)
	}
	var resp UnsealResponse
	wm, err := op.c.put(path, req, &resp, w)
	if err != nil {
		return nil, nil, err
	}
	return resp.Status, wm, nil
}

// KeyringRewrapRequest moves the keys of the region to another seal.
type KeyringRewrapRequest struct {
	// To is the seal type: aead or shares.
	To string

	// Shares and Threshold configure a new shares seal; zero uses the
	// leader's configuration or the defaults.
	Shares    int
	Threshold int
}

// KeyringRewrapResponse is the started rewrap. Shares are the unseal shares
// of a new shares seal: they are returned once and stored nowhere.
type KeyringRewrapResponse struct {
	RewrapID string
	Shares   []string
	Config   *SealConfig
}

// Rewrap starts a rewrap of the keys of the region to another seal.
func (k *Keyring) Rewrap(req *KeyringRewrapRequest, w *WriteOptions) (*KeyringRewrapResponse, *WriteMeta, error) {
	var resp KeyringRewrapResponse
	wm, err := k.client.put("/v1/operator/keyring/rewrap", req, &resp, w)
	if err != nil {
		return nil, nil, err
	}
	return &resp, wm, nil
}
