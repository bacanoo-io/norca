// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package secrets

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/nomad/structs"
)

// norcaWatchQueryTime bounds a blocking query of the metadata watch, so that
// a canceled watch returns within this time.
var norcaWatchQueryTime = 1 * time.Minute

// NorcaProvider is the built-in secret store, read over server RPC with the
// task's workload identity. Changes are watched with blocking queries on
// the secret's metadata; the value is fetched only when the version changes.
// The servers audit every value read, so the client records none.
type NorcaProvider struct {
	rpc    config.RPCer
	region string
}

// NewNorcaProvider returns the built-in store provider.
func NewNorcaProvider(rpc config.RPCer, region string) *NorcaProvider {
	return &NorcaProvider{rpc: rpc, region: region}
}

func (p *NorcaProvider) Name() string { return structs.SecretProviderNorca }
func (p *NorcaProvider) Type() string { return structs.SecretProviderNorca }

func (p *NorcaProvider) Capabilities() Capabilities {
	return Capabilities{Versions: true, Watch: true, AuditsReads: true}
}

func (p *NorcaProvider) query(id *Identity) structs.QueryOptions {
	q := structs.QueryOptions{Region: p.region}
	if id != nil {
		q.Namespace = id.Namespace
		q.AuthToken = id.Token
	}
	return q
}

func (p *NorcaProvider) check(req *Request) error {
	if p.rpc == nil {
		return NewError(p.Name(), KindUnavailable, errors.New("no server connection"))
	}
	if req.Engine != "" || req.Role != "" {
		return NewError(p.Name(), KindInvalid, errors.New("dynamic secrets are not supported yet"))
	}
	if err := structs.ValidateSecretPath(req.Path); err != nil {
		return NewError(p.Name(), KindInvalid, err)
	}
	return nil
}

// Fetch reads the value of the secret.
func (p *NorcaProvider) Fetch(ctx context.Context, req *Request) (*Secret, error) {
	if err := p.check(req); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	args := &structs.SecretReadRequest{
		Path:         req.Path,
		Version:      req.Version,
		QueryOptions: p.query(req.Identity),
	}
	var reply structs.SecretReadResponse
	if err := p.rpc.RPC(structs.SecretReadRPCMethod, args, &reply); err != nil {
		return nil, p.classify(err)
	}
	if reply.Secret == nil || reply.Items == nil {
		return nil, NewError(p.Name(), KindNotFound, fmt.Errorf("secret %q not found", req.Path))
	}
	return &Secret{
		Items:    map[string]string(reply.Items.Copy()),
		Version:  strconv.FormatUint(reply.Version, 10),
		Metadata: norcaMetadata(reply.Secret, reply.Version),
	}, nil
}

// ReadMetadata reads the metadata of the secret, without its value.
func (p *NorcaProvider) ReadMetadata(ctx context.Context, req *Request) (*Metadata, error) {
	if err := p.check(req); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	args := &structs.SecretReadMetadataRequest{Path: req.Path, QueryOptions: p.query(req.Identity)}
	var reply structs.SecretMetadataResponse
	if err := p.rpc.RPC(structs.SecretReadMetadataRPCMethod, args, &reply); err != nil {
		return nil, p.classify(err)
	}
	if reply.Secret == nil {
		return nil, NewError(p.Name(), KindNotFound, fmt.Errorf("secret %q not found", req.Path))
	}
	return norcaMetadata(reply.Secret, reply.Secret.CurrentVersion), nil
}

// norcaMetadata returns the metadata of a secret of the built-in store and
// of its version.
func norcaMetadata(s *structs.Secret, version uint64) *Metadata {
	if s == nil {
		return nil
	}
	nanos := func(n int64) time.Time {
		if n == 0 {
			return time.Time{}
		}
		return time.Unix(0, n).UTC()
	}
	md := &Metadata{
		Version:        version,
		CurrentVersion: s.CurrentVersion,
		MaxVersions:    s.MaxVersions,
		CreateTime:     nanos(s.CreateTime),
		UpdateTime:     nanos(s.ModifyTime),
		Deleted:        s.Deleted || s.Destroyed,
	}
	if len(s.Labels) > 0 {
		md.Labels = make(map[string]string, len(s.Labels))
		for k, v := range s.Labels {
			md.Labels[k] = v
		}
	}
	for _, v := range s.Versions {
		if v == nil {
			continue
		}
		if md.OldestVersion == 0 || v.Version < md.OldestVersion {
			md.OldestVersion = v.Version
		}
		md.Versions = append(md.Versions, &VersionMetadata{
			Version:     v.Version,
			CreatedTime: nanos(v.CreateTime),
			Deleted:     v.Deleted,
			Destroyed:   v.Destroyed,
		})
		if v.Version == version {
			md.CreatedTime = nanos(v.CreateTime)
		}
	}
	return md
}

// Watch blocks on the secret's metadata until its current version differs
// from version. Soft deleted and destroyed current versions are reported as
// a version of their own, so the value is fetched again (and fails).
func (p *NorcaProvider) Watch(ctx context.Context, req *Request, version string) (string, error) {
	if err := p.check(req); err != nil {
		return "", err
	}
	var index uint64
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		args := &structs.SecretReadMetadataRequest{Path: req.Path, QueryOptions: p.query(req.Identity)}
		args.AllowStale = true
		args.MinQueryIndex = index
		args.MaxQueryTime = norcaWatchQueryTime
		var reply structs.SecretMetadataResponse
		if err := p.rpc.RPC(structs.SecretReadMetadataRPCMethod, args, &reply); err != nil {
			return "", p.classify(err)
		}
		current := ""
		if s := reply.Secret; s != nil {
			current = strconv.FormatUint(s.CurrentVersion, 10)
			if s.Deleted || s.Destroyed {
				current += ":deleted"
			}
		}
		if current != version {
			return current, nil
		}
		if reply.Index > index {
			index = reply.Index
		} else if index == 0 {
			index = 1
		}
	}
}

// classify maps an RPC error to a provider error.
func (p *NorcaProvider) classify(err error) error {
	msg := err.Error()
	if code, _, ok := structs.CodeFromRPCCodedErr(err); ok {
		switch code {
		case http.StatusNotFound:
			return NewError(p.Name(), KindNotFound, err)
		case http.StatusBadRequest:
			return NewError(p.Name(), KindInvalid, err)
		case http.StatusForbidden:
			return NewError(p.Name(), KindDenied, err)
		}
		return NewError(p.Name(), KindUnavailable, err)
	}
	if strings.Contains(msg, structs.ErrPermissionDenied.Error()) ||
		strings.Contains(msg, structs.ErrTokenNotFound.Error()) {
		return NewError(p.Name(), KindDenied, err)
	}
	return NewError(p.Name(), KindUnavailable, err)
}
