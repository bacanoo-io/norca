// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"net/url"
	"strconv"
	"time"
)

// SecretEngineTypeOperator is an engine served by an operator.
const SecretEngineTypeOperator = "operator"

// SecretEngines is used to manage the dynamic secrets engines of the
// built-in secret store. An engine names an operator: a service that creates
// the credentials of its target system and keeps its admin credential.
type SecretEngines struct {
	client *Client
}

// SecretEngines returns a new handle on the secret engines.
func (c *Client) SecretEngines() *SecretEngines {
	return &SecretEngines{client: c}
}

// SecretEngine is a dynamic secrets engine of a namespace.
type SecretEngine struct {
	Namespace string `json:",omitempty"`
	Name      string

	// Type is the backend type ("operator").
	Type        string
	Description string `json:",omitempty"`

	Operator *SecretEngineOperator `json:",omitempty"`
	Roles    []*SecretEngineRole   `json:",omitempty"`

	CreateIndex uint64 `json:",omitempty"`
	ModifyIndex uint64 `json:",omitempty"`
	CreateTime  int64  `json:",omitempty"`
	ModifyTime  int64  `json:",omitempty"`
}

// SecretEngineRole is a role of an engine.
type SecretEngineRole struct {
	Name   string
	TTL    time.Duration `json:",omitempty"`
	MaxTTL time.Duration `json:",omitempty"`

	// Config is passed to the operator of an engine; it must hold nothing
	// secret.
	Config map[string]string `json:",omitempty"`

	// Params are the keys of the params a job may send to the operator.
	Params []string `json:",omitempty"`
}

// SecretEngineOperator names the service of the operator of an engine.
type SecretEngineOperator struct {
	Service   string
	Namespace string `json:",omitempty"`
}

// SecretEngineStub is the list form of an engine.
type SecretEngineStub struct {
	Namespace   string
	Name        string
	Type        string
	Description string
	Roles       []string
	CreateIndex uint64
	ModifyIndex uint64
	CreateTime  int64
	ModifyTime  int64
}

// secretEngineWriteResult is the body of the response of an engine write.
type secretEngineWriteResult struct {
	Engine *SecretEngine
}

func secretEngineEndpoint(name string) string {
	return "/v1/secret-engine/" + url.PathEscape(name)
}

// List lists the engines of the namespace of the query options whose name
// starts with prefix.
func (s *SecretEngines) List(prefix string, q *QueryOptions) ([]*SecretEngineStub, *QueryMeta, error) {
	if q == nil {
		q = &QueryOptions{}
	}
	qc := *q
	if prefix != "" {
		qc.Prefix = prefix
	}
	var out []*SecretEngineStub
	qm, err := s.client.query("/v1/secret-engines", &out, &qc)
	if err != nil {
		return nil, nil, err
	}
	return out, qm, nil
}

// Info reads an engine.
func (s *SecretEngines) Info(name string, q *QueryOptions) (*SecretEngine, *QueryMeta, error) {
	var out SecretEngine
	qm, err := s.client.query(secretEngineEndpoint(name), &out, q)
	if err != nil {
		return nil, nil, err
	}
	return &out, qm, nil
}

// Write creates or updates an engine. The servers validate it with its
// admin credential and configure the backend before it is written.
func (s *SecretEngines) Write(engine *SecretEngine, q *WriteOptions) (*SecretEngine, *WriteMeta, error) {
	return s.write(secretEngineEndpoint(engine.Name), engine, q)
}

// CheckedWrite writes an engine if its modify index is checkIndex; 0
// creates an engine that must not exist yet.
func (s *SecretEngines) CheckedWrite(engine *SecretEngine, checkIndex uint64, q *WriteOptions) (*SecretEngine, *WriteMeta, error) {
	return s.write(secretEngineEndpoint(engine.Name)+"?cas="+strconv.FormatUint(checkIndex, 10), engine, q)
}

func (s *SecretEngines) write(endpoint string, engine *SecretEngine, q *WriteOptions) (*SecretEngine, *WriteMeta, error) {
	var out secretEngineWriteResult
	wm, err := s.client.put(endpoint, engine, &out, q)
	if err != nil {
		return nil, nil, err
	}
	return out.Engine, wm, nil
}

// Delete deletes an engine; it fails while the engine has live leases.
func (s *SecretEngines) Delete(name string, q *WriteOptions) (*WriteMeta, error) {
	return s.client.delete(secretEngineEndpoint(name), nil, nil, q)
}

// RevokeLeases revokes every live lease of an engine and returns their IDs.
func (s *SecretEngines) RevokeLeases(name string, q *WriteOptions) ([]string, *WriteMeta, error) {
	var out secretLeaseRevokeResult
	wm, err := s.client.post(secretEngineEndpoint(name)+"/revoke", nil, &out, q)
	if err != nil {
		return nil, nil, err
	}
	return out.Revoked, wm, nil
}

// SecretLeases is used to read and revoke the leases of dynamic secrets.
type SecretLeases struct {
	client *Client
}

// SecretLeases returns a new handle on the leases of dynamic secrets.
func (c *Client) SecretLeases() *SecretLeases {
	return &SecretLeases{client: c}
}

// SecretLease is the lease of a credential created by an engine for a
// task. It never contains the credential.
type SecretLease struct {
	ID            string
	Namespace     string
	Engine        string
	EngineType    string
	Role          string
	SecretName    string
	AllocID       string
	JobID         string
	TaskGroup     string
	Task          string
	NodeID        string
	TTL           time.Duration
	MaxTTL        time.Duration
	IssueTime     int64
	ExpireTime    int64
	MaxExpireTime int64
	RenewTime     int64 `json:",omitempty"`
	Renewals      int   `json:",omitempty"`
	Revoked       bool
	RevokeTime    int64  `json:",omitempty"`
	RevokeReason  string `json:",omitempty"`

	// BackendRef identifies the credential at the operator.
	BackendRef  string
	CreateIndex uint64
	ModifyIndex uint64
}

type secretLeaseRevokeResult struct {
	Revoked []string
}

// List lists the leases of the namespace of the query options, of one
// engine if engine is not empty.
func (s *SecretLeases) List(engine string, q *QueryOptions) ([]*SecretLease, *QueryMeta, error) {
	endpoint := "/v1/secret-leases"
	if engine != "" {
		endpoint += "?engine=" + url.QueryEscape(engine)
	}
	var out []*SecretLease
	qm, err := s.client.query(endpoint, &out, q)
	if err != nil {
		return nil, nil, err
	}
	return out, qm, nil
}

// Info reads a lease.
func (s *SecretLeases) Info(id string, q *QueryOptions) (*SecretLease, *QueryMeta, error) {
	var out SecretLease
	qm, err := s.client.query("/v1/secret-lease/"+url.PathEscape(id), &out, q)
	if err != nil {
		return nil, nil, err
	}
	return &out, qm, nil
}

// Revoke revokes a lease.
func (s *SecretLeases) Revoke(id string, q *WriteOptions) ([]string, *WriteMeta, error) {
	var out secretLeaseRevokeResult
	wm, err := s.client.post("/v1/secret-lease/"+url.PathEscape(id)+"/revoke", nil, &out, q)
	if err != nil {
		return nil, nil, err
	}
	return out.Revoked, wm, nil
}
