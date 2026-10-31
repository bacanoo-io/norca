// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package config

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strings"

	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/nomad/helper/pointer"
)

// Defaults of the consul_compat block of the client configuration.
const (
	DefaultConsulCompatBind = "127.0.0.1:8500"

	// ConsulCompatDatacenterFromRegion maps Consul datacenters to Nomad
	// regions.
	ConsulCompatDatacenterFromRegion = "region"

	// ConsulCompatDatacenterFromDatacenter maps Consul datacenters to Nomad
	// datacenters of the local region.
	ConsulCompatDatacenterFromDatacenter = "datacenter"

	// ConsulCompatKVAuto stores the Consul KV on the system bus of the
	// servers when they run one, else in Variables. The choice is made once,
	// when the first request finds out which servers the client has.
	ConsulCompatKVAuto = "auto"

	// ConsulCompatKVBus stores the Consul KV in the key-value bucket
	// consul-kv of the system bus of the servers (embedded NATS).
	ConsulCompatKVBus = "bus"

	// ConsulCompatKVVariables stores the Consul KV in Norca Variables.
	ConsulCompatKVVariables = "variables"

	// ConsulCompatKVNATS stores the Consul KV in a NATS JetStream key-value
	// bucket.
	ConsulCompatKVNATS = "nats"

	// ConsulCompatKVDisabled turns the KV endpoints off.
	ConsulCompatKVDisabled = "disabled"

	// DefaultConsulCompatKVPath is the Variables path the Consul keys live
	// under; the ACL rules of both backends apply to it.
	DefaultConsulCompatKVPath = "consul/kv"

	// DefaultConsulCompatKVBucket is the JetStream bucket of the nats
	// backend.
	DefaultConsulCompatKVBucket = "consul-kv"
)

// ConsulCompatConfig is the consul_compat block of the client configuration:
// the read-only HTTP API compatible with the catalog and health endpoints of
// Consul, for tools that only know Consul.
type ConsulCompatConfig struct {
	// Enabled starts the API. Defaults to false.
	Enabled *bool `hcl:"enabled"`

	// Bind is the address the API listens on. Defaults to 127.0.0.1:8500.
	Bind string `hcl:"bind"`

	// DatacenterFrom is "region" (default) to show Nomad regions as Consul
	// datacenters, or "datacenter" to show the Nomad datacenters of the
	// local region.
	DatacenterFrom string `hcl:"datacenter_from"`

	// Namespaces are the Nomad namespaces shown. The services of the first
	// one appear under their name, those of the others as
	// "<namespace>--<name>". Defaults to ["default"].
	Namespaces []string `hcl:"namespaces"`

	// AnonymousToken is the Nomad ACL token used for requests that carry no
	// token. It should be a read-only token.
	AnonymousToken string `hcl:"anonymous_token"`

	// KV configures the Consul KV endpoints (/v1/kv).
	KV *ConsulCompatKVConfig `hcl:"kv"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

// ConsulCompatKVConfig is the kv block of consul_compat: where the Consul
// key-value store of the API lives.
type ConsulCompatKVConfig struct {
	// Backend is "auto" (default: "bus" when the servers run a system bus,
	// else "variables"), "bus", "variables", "nats" (an external NATS) or
	// "disabled".
	Backend string `hcl:"backend"`

	// Namespace of the keys: of the Variables, and of the ACL rules of both
	// backends. Defaults to the first namespace of consul_compat.
	Namespace string `hcl:"namespace"`

	// Path is the Variables path the keys live under (default
	// "consul/kv"): key "app/config" is the Variable
	// "consul/kv/app/config". The ACL rules of the path apply to the nats
	// backend as well.
	Path string `hcl:"path"`

	// NATS configures the nats backend.
	NATS *ConsulCompatKVNATSConfig `hcl:"nats"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

// ConsulCompatKVNATSConfig is the connection and the bucket of the nats
// backend of the Consul KV.
type ConsulCompatKVNATSConfig struct {
	// URLs of the NATS servers, e.g. ["nats://10.0.0.1:4222"].
	URLs []string `hcl:"urls"`

	// Bucket is the JetStream key-value bucket (default "consul-kv").
	Bucket string `hcl:"bucket"`

	// CreateBucket creates the bucket when it is missing (default true).
	CreateBucket *bool `hcl:"create_bucket"`

	// Replicas of a created bucket (default 1).
	Replicas int `hcl:"replicas"`

	// Authentication: a credentials file, an NKey seed file, a user and
	// password, or a token.
	CredentialsFile string `hcl:"credentials_file"`
	NKeySeedFile    string `hcl:"nkey_seed_file"`
	User            string `hcl:"user"`
	Password        string `hcl:"password"`
	Token           string `hcl:"token"`

	// TLS: the CA of the servers, and a client certificate when the servers
	// verify clients.
	CAFile   string `hcl:"ca_file"`
	CertFile string `hcl:"cert_file"`
	KeyFile  string `hcl:"key_file"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

// Copy returns a copy of c.
func (c *ConsulCompatKVConfig) Copy() *ConsulCompatKVConfig {
	if c == nil {
		return nil
	}
	nc := *c
	nc.NATS = c.NATS.Copy()
	nc.ExtraKeysHCL = nil
	return &nc
}

// Merge returns c with the values set in o applied.
func (c *ConsulCompatKVConfig) Merge(o *ConsulCompatKVConfig) *ConsulCompatKVConfig {
	switch {
	case c == nil:
		return o.Copy()
	case o == nil:
		return c.Copy()
	}
	r := c.Copy()
	if o.Backend != "" {
		r.Backend = o.Backend
	}
	if o.Namespace != "" {
		r.Namespace = o.Namespace
	}
	if o.Path != "" {
		r.Path = o.Path
	}
	if o.NATS != nil {
		r.NATS = o.NATS.Copy()
	}
	return r
}

// BackendName returns the effective backend.
func (c *ConsulCompatKVConfig) BackendName() string {
	if c == nil || c.Backend == "" {
		return ConsulCompatKVAuto
	}
	return c.Backend
}

// PathPrefix returns the effective Variables path of the keys.
func (c *ConsulCompatKVConfig) PathPrefix() string {
	if c == nil || c.Path == "" {
		return DefaultConsulCompatKVPath
	}
	return strings.Trim(c.Path, "/")
}

// Validate the kv block.
func (c *ConsulCompatKVConfig) Validate() error {
	if c == nil {
		return nil
	}
	var mErr multierror.Error
	switch c.Backend {
	case "", ConsulCompatKVAuto, ConsulCompatKVBus, ConsulCompatKVVariables, ConsulCompatKVDisabled:
	case ConsulCompatKVNATS:
		if c.NATS == nil || len(c.NATS.URLs) == 0 {
			mErr.Errors = append(mErr.Errors, errors.New(`consul_compat kv backend "nats" needs nats.urls`))
		}
	default:
		mErr.Errors = append(mErr.Errors, fmt.Errorf("consul_compat kv backend must be %q, %q, %q, %q or %q, got %q",
			ConsulCompatKVAuto, ConsulCompatKVBus, ConsulCompatKVVariables, ConsulCompatKVNATS, ConsulCompatKVDisabled, c.Backend))
	}
	if c.Path != "" && !validKVPath.MatchString(c.PathPrefix()) {
		mErr.Errors = append(mErr.Errors, fmt.Errorf(
			"consul_compat kv path %q must be a Variables path of at most 64 characters (letters, digits, -, _, ~ and /)", c.Path))
	}
	if n := c.NATS; n != nil {
		if n.Replicas < 0 {
			mErr.Errors = append(mErr.Errors, errors.New("consul_compat kv nats replicas must not be negative"))
		}
		if (n.CertFile == "") != (n.KeyFile == "") {
			mErr.Errors = append(mErr.Errors, errors.New("consul_compat kv nats cert_file and key_file must be set together"))
		}
	}
	return mErr.ErrorOrNil()
}

// validKVPath is the form of the Variables path of the keys; it leaves at
// least 63 of the 128 characters of a Variables path to the keys.
var validKVPath = regexp.MustCompile(`^[a-zA-Z0-9-_~/]{1,64}$`)

// Copy returns a copy of c.
func (c *ConsulCompatKVNATSConfig) Copy() *ConsulCompatKVNATSConfig {
	if c == nil {
		return nil
	}
	nc := *c
	nc.URLs = slices.Clone(c.URLs)
	nc.CreateBucket = pointer.Copy(c.CreateBucket)
	nc.ExtraKeysHCL = nil
	return &nc
}

// BucketName returns the effective bucket.
func (c *ConsulCompatKVNATSConfig) BucketName() string {
	if c == nil || c.Bucket == "" {
		return DefaultConsulCompatKVBucket
	}
	return c.Bucket
}

// CreatesBucket returns whether a missing bucket is created.
func (c *ConsulCompatKVNATSConfig) CreatesBucket() bool {
	return c == nil || c.CreateBucket == nil || *c.CreateBucket
}

// Copy returns a copy of c.
func (c *ConsulCompatConfig) Copy() *ConsulCompatConfig {
	if c == nil {
		return nil
	}
	nc := *c
	nc.Enabled = pointer.Copy(c.Enabled)
	nc.Namespaces = slices.Clone(c.Namespaces)
	nc.KV = c.KV.Copy()
	nc.ExtraKeysHCL = nil
	return &nc
}

// Merge returns c with the values set in o applied.
func (c *ConsulCompatConfig) Merge(o *ConsulCompatConfig) *ConsulCompatConfig {
	switch {
	case c == nil:
		return o.Copy()
	case o == nil:
		return c.Copy()
	}
	r := c.Copy()
	if o.Enabled != nil {
		r.Enabled = pointer.Copy(o.Enabled)
	}
	if o.Bind != "" {
		r.Bind = o.Bind
	}
	if o.DatacenterFrom != "" {
		r.DatacenterFrom = o.DatacenterFrom
	}
	if len(o.Namespaces) > 0 {
		r.Namespaces = slices.Clone(o.Namespaces)
	}
	if o.AnonymousToken != "" {
		r.AnonymousToken = o.AnonymousToken
	}
	if o.KV != nil {
		r.KV = r.KV.Merge(o.KV)
	}
	return r
}

// IsEnabled returns whether the API is enabled.
func (c *ConsulCompatConfig) IsEnabled() bool {
	return c != nil && c.Enabled != nil && *c.Enabled
}

// BindAddr returns the effective bind address.
func (c *ConsulCompatConfig) BindAddr() string {
	if c == nil || c.Bind == "" {
		return DefaultConsulCompatBind
	}
	return c.Bind
}

// DatacenterIsRegion returns whether Consul datacenters are Nomad regions.
func (c *ConsulCompatConfig) DatacenterIsRegion() bool {
	return c == nil || c.DatacenterFrom == "" || c.DatacenterFrom == ConsulCompatDatacenterFromRegion
}

// NamespaceList returns the effective namespaces.
func (c *ConsulCompatConfig) NamespaceList() []string {
	if c == nil || len(c.Namespaces) == 0 {
		return []string{"default"}
	}
	return c.Namespaces
}

// Validate the consul_compat block.
func (c *ConsulCompatConfig) Validate() error {
	if c == nil {
		return nil
	}
	var mErr multierror.Error
	if _, _, err := net.SplitHostPort(c.BindAddr()); err != nil {
		mErr.Errors = append(mErr.Errors, fmt.Errorf("consul_compat bind must be host:port: %v", err))
	}
	switch c.DatacenterFrom {
	case "", ConsulCompatDatacenterFromRegion, ConsulCompatDatacenterFromDatacenter:
	default:
		mErr.Errors = append(mErr.Errors, fmt.Errorf("consul_compat datacenter_from must be %q or %q, got %q",
			ConsulCompatDatacenterFromRegion, ConsulCompatDatacenterFromDatacenter, c.DatacenterFrom))
	}
	for _, ns := range c.Namespaces {
		if ns == "" {
			mErr.Errors = append(mErr.Errors, errors.New("consul_compat namespaces must not be empty"))
		}
	}
	if err := c.KV.Validate(); err != nil {
		mErr.Errors = append(mErr.Errors, err)
	}
	return mErr.ErrorOrNil()
}
