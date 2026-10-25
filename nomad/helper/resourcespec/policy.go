// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package resourcespec parses the specification files of Norca resources:
// namespaces, quotas, Variables, ACL policies, admission policies, object
// kinds and objects. The CLI apply commands and norca pack share these
// parsers, so a resource template of a pack has the format of the file the
// corresponding apply command reads.
package resourcespec

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hashicorp/hcl"
	"github.com/hashicorp/hcl/hcl/ast"
	"github.com/hashicorp/hcl/v2/hclsimple"
	"github.com/hashicorp/nomad/api"
)

// aclPolicyFile is the HCL form of an ACL policy with its metadata:
//
//	policy "redis-read" {
//	  description = "Read access for the redis pack"
//	  rules       = <<EOT
//	namespace "default" { policy = "read" }
//	EOT
//	  job_acl {
//	    namespace = "default"
//	    job_id    = "redis"
//	  }
//	}
type aclPolicyFile struct {
	Policy struct {
		Name        string `hcl:"name,label"`
		Description string `hcl:"description,optional"`
		Rules       string `hcl:"rules"`
		JobACL      *struct {
			Namespace string `hcl:"namespace,optional"`
			JobID     string `hcl:"job_id"`
			Group     string `hcl:"group,optional"`
			Task      string `hcl:"task,optional"`
		} `hcl:"job_acl,block"`
	} `hcl:"policy,block"`
}

// ParseACLPolicy parses an ACL policy with its name, description, rules
// and optional job binding (see aclPolicyFile), in HCL or, for .json files,
// in the API JSON form.
func ParseACLPolicy(path string, content []byte) (*api.ACLPolicy, error) {
	if isJSONInput(path, false) {
		var p api.ACLPolicy
		if err := json.Unmarshal(content, &p); err != nil {
			return nil, err
		}
		return &p, nil
	}
	var f aclPolicyFile
	if err := hclsimple.Decode(hclFileName(path), content, nil, &f); err != nil {
		return nil, err
	}
	p := &api.ACLPolicy{
		Name:        f.Policy.Name,
		Description: f.Policy.Description,
		Rules:       f.Policy.Rules,
	}
	if j := f.Policy.JobACL; j != nil {
		p.JobACL = &api.JobACL{Namespace: j.Namespace, JobID: j.JobID, Group: j.Group, Task: j.Task}
	}
	return p, nil
}

// ParseAdmissionPolicy parses an admission policy in HCL or, for .json
// files, in the API JSON form.
func ParseAdmissionPolicy(path string, content []byte) (*api.AdmissionPolicy, error) {
	var policy api.AdmissionPolicy
	if isJSONInput(path, false) {
		if err := json.Unmarshal(content, &policy); err != nil {
			return nil, err
		}
		return &policy, nil
	}
	if err := hclsimple.Decode(hclFileName(path), content, nil, &policy); err != nil {
		return nil, err
	}
	return &policy, nil
}

// ParseVariableWithSecrets parses a Variables specification like
// ParseVariable, with an additional top-level attribute secret_items: the
// names of items whose values are secret. Secret items must not have a
// value in the specification.
func ParseVariableWithSecrets(input []byte) (*api.Variable, []string, error) {
	root, err := hcl.ParseBytes(input)
	if err != nil {
		return nil, nil, err
	}
	list, ok := root.Node.(*ast.ObjectList)
	if !ok {
		return nil, nil, fmt.Errorf("error parsing: root should be an object")
	}

	var secrets []string
	rest := &ast.ObjectList{}
	for _, item := range list.Items {
		if len(item.Keys) == 1 && item.Keys[0].Token.Value() == "secret_items" {
			if err := hcl.DecodeObject(&secrets, item.Val); err != nil {
				return nil, nil, fmt.Errorf("secret_items: %w", err)
			}
			continue
		}
		rest.Add(item)
	}

	var out api.Variable
	if err := parseVariableSpecImpl(&out, rest); err != nil {
		return nil, nil, err
	}
	for _, k := range secrets {
		if _, ok := out.Items[k]; ok {
			return nil, nil, fmt.Errorf("secret item %q must not have a value in the specification", k)
		}
	}
	return &out, secrets, nil
}

// hclFileName gives a file name the .hcl extension hclsimple needs.
func hclFileName(path string) string {
	if strings.HasSuffix(path, ".hcl") {
		return path
	}
	return path + ".hcl"
}
