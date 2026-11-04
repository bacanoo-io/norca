// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package externalspec reads and writes the file form of external services:
// the HCL "external_service" blocks of "norca service apply" and of the HCL
// editor of the web UI, and their JSON form.
//
//	external_service "orders-db" {
//	  namespace = "prod"
//	  address   = "10.20.0.5"
//	  port      = 5432
//
//	  check {
//	    type = "tcp"
//	    tcp  = "10.20.0.5:5432"
//	  }
//
//	  checked_by {
//	    node_pool = "edge"
//	  }
//	}
package externalspec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsimple"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"

	"github.com/bacanoo-io/norca/norca/api"
)

// Item is an external service of a file, with the namespace it is declared
// in (empty: the namespace of the request).
type Item struct {
	Namespace string
	Service   *api.ExternalService
}

// Problem is one error of a file with the line it is on (0 if unknown).
type Problem struct {
	Message string
	Line    int
}

// Error is the list of problems of a file.
type Error struct {
	Problems []Problem
}

func (e *Error) Error() string {
	msgs := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		if p.Line > 0 {
			msgs[i] = fmt.Sprintf("line %d: %s", p.Line, p.Message)
		} else {
			msgs[i] = p.Message
		}
	}
	return strings.Join(msgs, "; ")
}

func problemf(format string, args ...any) error {
	return &Error{Problems: []Problem{{Message: fmt.Sprintf(format, args...)}}}
}

type fileSpec struct {
	Services []serviceSpec `hcl:"external_service,block"`
}

type serviceSpec struct {
	Name                    string            `hcl:"name,label"`
	ID                      string            `hcl:"id,optional"`
	Namespace               string            `hcl:"namespace,optional"`
	Type                    string            `hcl:"type,optional"`
	Address                 string            `hcl:"address,optional"`
	Target                  string            `hcl:"target,optional"`
	Port                    int               `hcl:"port,optional"`
	Tags                    []string          `hcl:"tags,optional"`
	Meta                    map[string]string `hcl:"meta,optional"`
	Datacenter              string            `hcl:"datacenter,optional"`
	DeregisterCriticalAfter *string           `hcl:"deregister_critical_after,optional"`
	Checks                  []checkSpec       `hcl:"check,block"`
	CheckedBy               *checkedBySpec    `hcl:"checked_by,block"`
}

type checkSpec struct {
	ID            string              `hcl:"id,optional"`
	Name          string              `hcl:"name,optional"`
	Type          string              `hcl:"type"`
	Notes         string              `hcl:"notes,optional"`
	TTL           *string             `hcl:"ttl,optional"`
	HTTP          string              `hcl:"http,optional"`
	Method        string              `hcl:"method,optional"`
	Header        map[string][]string `hcl:"header,optional"`
	Body          string              `hcl:"body,optional"`
	TLSSkipVerify bool                `hcl:"tls_skip_verify,optional"`
	TLSServerName string              `hcl:"tls_server_name,optional"`
	TCP           string              `hcl:"tcp,optional"`
	Interval      *string             `hcl:"interval,optional"`
	Timeout       *string             `hcl:"timeout,optional"`
	InitialStatus string              `hcl:"initial_status,optional"`
}

type checkedBySpec struct {
	NodePool    string   `hcl:"node_pool,optional"`
	Datacenters []string `hcl:"datacenters,optional"`
}

// Parse reads the HCL (or, with jsonInput, the API JSON) form of a file. The
// services it returns are marked Declared.
func Parse(path string, content []byte, jsonInput bool) ([]*Item, error) {
	var items []*Item
	var err error
	if jsonInput {
		items, err = parseJSON(content)
	} else {
		items, err = parseHCL(path, content)
	}
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, problemf("no external service found")
	}
	seen := make(map[string]bool)
	for _, it := range items {
		it.Service.Declared = true
		id := it.Service.ID
		if id == "" {
			id = it.Service.Name
		}
		key := it.Namespace + "/" + id
		if seen[key] {
			return nil, problemf("external service %q is declared twice", id)
		}
		seen[key] = true
	}
	return items, nil
}

func parseJSON(content []byte) ([]*Item, error) {
	type jsonItem struct {
		Namespace string
		api.ExternalService
	}
	var list []jsonItem
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return nil, problemf("%v", err)
		}
	} else {
		var one jsonItem
		if err := json.Unmarshal(trimmed, &one); err != nil {
			return nil, problemf("%v", err)
		}
		list = []jsonItem{one}
	}
	items := make([]*Item, 0, len(list))
	for i := range list {
		svc := list[i].ExternalService
		if svc.Name == "" {
			return nil, problemf("external service %d has no Name", i+1)
		}
		items = append(items, &Item{Namespace: list[i].Namespace, Service: &svc})
	}
	return items, nil
}

func parseHCL(path string, content []byte) ([]*Item, error) {
	if !strings.HasSuffix(path, ".hcl") {
		path += ".hcl"
	}
	var file fileSpec
	if err := hclsimple.Decode(path, content, nil, &file); err != nil {
		if diags, ok := err.(hcl.Diagnostics); ok {
			e := &Error{}
			for _, d := range diags {
				if d.Severity != hcl.DiagError {
					continue
				}
				p := Problem{Message: d.Summary}
				if d.Detail != "" {
					p.Message += ": " + d.Detail
				}
				if d.Subject != nil {
					p.Line = d.Subject.Start.Line
				}
				e.Problems = append(e.Problems, p)
			}
			return nil, e
		}
		return nil, problemf("%v", err)
	}

	items := make([]*Item, 0, len(file.Services))
	for i := range file.Services {
		item, err := file.Services[i].toAPI()
		if err != nil {
			return nil, problemf("external service %q: %v", file.Services[i].Name, err)
		}
		items = append(items, item)
	}
	return items, nil
}

func parseDuration(name string, v *string) (time.Duration, error) {
	if v == nil || *v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(*v)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q", name, *v)
	}
	return d, nil
}

func (s *serviceSpec) toAPI() (*Item, error) {
	svc := &api.ExternalService{
		ID:         s.ID,
		Name:       s.Name,
		Port:       s.Port,
		Tags:       s.Tags,
		Meta:       s.Meta,
		Datacenter: s.Datacenter,
	}
	switch s.Type {
	case "", api.ExternalServiceKindAddress:
		if s.Target != "" {
			return nil, fmt.Errorf(`"target" is for type "alias"; use "address"`)
		}
		svc.Kind = api.ExternalServiceKindAddress
		svc.Address = s.Address
	case api.ExternalServiceKindAlias:
		if s.Address != "" {
			return nil, fmt.Errorf(`"address" is for type "address"; use "target"`)
		}
		svc.Kind = api.ExternalServiceKindAlias
		svc.Target = s.Target
	default:
		return nil, fmt.Errorf(`invalid type %q (address or alias)`, s.Type)
	}
	if s.DeregisterCriticalAfter != nil {
		d, err := parseDuration("deregister_critical_after", s.DeregisterCriticalAfter)
		if err != nil {
			return nil, err
		}
		svc.DeregisterCriticalAfter = &d
	}
	for _, c := range s.Checks {
		check := &api.ExternalServiceCheck{
			ID: c.ID, Name: c.Name, Type: c.Type, Notes: c.Notes, HTTP: c.HTTP, Method: c.Method,
			Header: c.Header, Body: c.Body, TLSSkipVerify: c.TLSSkipVerify, TLSServerName: c.TLSServerName,
			TCP: c.TCP, InitialStatus: c.InitialStatus,
		}
		var err error
		if check.TTL, err = parseDuration("ttl", c.TTL); err != nil {
			return nil, err
		}
		if check.Interval, err = parseDuration("interval", c.Interval); err != nil {
			return nil, err
		}
		if check.Timeout, err = parseDuration("timeout", c.Timeout); err != nil {
			return nil, err
		}
		svc.Checks = append(svc.Checks, check)
	}
	if s.CheckedBy != nil {
		svc.CheckedBy = &api.ExternalCheckedBy{NodePool: s.CheckedBy.NodePool, Datacenters: s.CheckedBy.Datacenters}
	}
	return &Item{Namespace: s.Namespace, Service: svc}, nil
}

func stringList(v []string) cty.Value {
	vals := make([]cty.Value, len(v))
	for i, s := range v {
		vals[i] = cty.StringVal(s)
	}
	return cty.ListVal(vals)
}

func setString(b *hclwrite.Body, name, v string) {
	if v != "" {
		b.SetAttributeValue(name, cty.StringVal(v))
	}
}

func setDuration(b *hclwrite.Body, name string, d time.Duration) {
	if d != 0 {
		b.SetAttributeValue(name, cty.StringVal(d.String()))
	}
}

// Format writes the HCL form of services; Parse reads it back to the same
// services.
func Format(items []*Item) []byte {
	f := hclwrite.NewEmptyFile()
	root := f.Body()
	for n, it := range items {
		if n > 0 {
			root.AppendNewline()
		}
		s := it.Service
		block := root.AppendNewBlock("external_service", []string{s.Name})
		b := block.Body()
		if s.ID != "" && s.ID != s.Name {
			setString(b, "id", s.ID)
		}
		setString(b, "namespace", it.Namespace)
		if s.Kind == api.ExternalServiceKindAlias || (s.Kind == "" && s.Target != "" && s.Address == "") {
			b.SetAttributeValue("type", cty.StringVal(api.ExternalServiceKindAlias))
			setString(b, "target", s.Target)
		} else {
			setString(b, "address", s.Address)
		}
		if s.Port != 0 {
			b.SetAttributeValue("port", cty.NumberIntVal(int64(s.Port)))
		}
		if len(s.Tags) > 0 {
			b.SetAttributeValue("tags", stringList(s.Tags))
		}
		if len(s.Meta) > 0 {
			keys := make([]string, 0, len(s.Meta))
			for k := range s.Meta {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			obj := make(map[string]cty.Value, len(keys))
			for _, k := range keys {
				obj[k] = cty.StringVal(s.Meta[k])
			}
			b.SetAttributeValue("meta", cty.ObjectVal(obj))
		}
		setString(b, "datacenter", s.Datacenter)
		if s.DeregisterCriticalAfter != nil {
			b.SetAttributeValue("deregister_critical_after", cty.StringVal(s.DeregisterCriticalAfter.String()))
		}
		for _, c := range s.Checks {
			b.AppendNewline()
			cb := b.AppendNewBlock("check", nil).Body()
			setString(cb, "id", c.ID)
			setString(cb, "name", c.Name)
			cb.SetAttributeValue("type", cty.StringVal(c.Type))
			setString(cb, "notes", c.Notes)
			setDuration(cb, "ttl", c.TTL)
			setString(cb, "http", c.HTTP)
			setString(cb, "method", c.Method)
			if len(c.Header) > 0 {
				keys := make([]string, 0, len(c.Header))
				for k := range c.Header {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				obj := make(map[string]cty.Value, len(keys))
				for _, k := range keys {
					obj[k] = stringList(c.Header[k])
				}
				cb.SetAttributeValue("header", cty.ObjectVal(obj))
			}
			setString(cb, "body", c.Body)
			if c.TLSSkipVerify {
				cb.SetAttributeValue("tls_skip_verify", cty.True)
			}
			setString(cb, "tls_server_name", c.TLSServerName)
			setString(cb, "tcp", c.TCP)
			setDuration(cb, "interval", c.Interval)
			setDuration(cb, "timeout", c.Timeout)
			setString(cb, "initial_status", c.InitialStatus)
		}
		if s.CheckedBy != nil {
			b.AppendNewline()
			cb := b.AppendNewBlock("checked_by", nil).Body()
			setString(cb, "node_pool", s.CheckedBy.NodePool)
			if len(s.CheckedBy.Datacenters) > 0 {
				cb.SetAttributeValue("datacenters", stringList(s.CheckedBy.Datacenters))
			}
		}
	}
	return hclwrite.Format(f.Bytes())
}
