// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

// Modified by Norca contributors (2026): moved from the command package
// (namespace_apply.go, quota_apply.go, var_put.go) so that norca pack can
// parse the same specifications; the functions were exported and renamed.

package resourcespec

import (
	"fmt"
	"strings"

	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/hcl"
	"github.com/hashicorp/hcl/hcl/ast"
	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/helper"
	"github.com/mitchellh/mapstructure"
)

// ParseNamespace is used to parse the namespace specification from HCL
func ParseNamespace(input []byte) (*api.Namespace, error) {
	root, err := hcl.ParseBytes(input)
	if err != nil {
		return nil, err
	}

	// Top-level item should be a list
	list, ok := root.Node.(*ast.ObjectList)
	if !ok {
		return nil, fmt.Errorf("error parsing: root should be an object")
	}

	var spec api.Namespace
	if err := parseNamespaceSpecImpl(&spec, list); err != nil {
		return nil, err
	}

	return &spec, nil
}

// parseNamespaceSpec parses the quota namespace taking as input the AST tree
func parseNamespaceSpecImpl(result *api.Namespace, list *ast.ObjectList) error {
	// Decode the full thing into a map[string]interface for ease
	var m map[string]interface{}
	if err := hcl.DecodeObject(&m, list); err != nil {
		return err
	}

	delete(m, "capabilities")
	delete(m, "meta")
	delete(m, "node_pool_config")

	// Decode the rest
	if err := mapstructure.WeakDecode(m, result); err != nil {
		return err
	}

	cObj := list.Filter("capabilities")
	if len(cObj.Items) > 0 {
		for _, o := range cObj.Elem().Items {
			ot, ok := o.Val.(*ast.ObjectType)
			if !ok {
				break
			}
			var opts *api.NamespaceCapabilities
			if err := hcl.DecodeObject(&opts, ot.List); err != nil {
				return err
			}
			result.Capabilities = opts
			break
		}
	}

	npObj := list.Filter("node_pool_config")
	if len(npObj.Items) > 0 {
		for _, o := range npObj.Elem().Items {
			ot, ok := o.Val.(*ast.ObjectType)
			if !ok {
				break
			}
			var npConfig *api.NamespaceNodePoolConfiguration
			if err := hcl.DecodeObject(&npConfig, ot.List); err != nil {
				return err
			}
			result.NodePoolConfiguration = npConfig
			break
		}
	}

	if metaO := list.Filter("meta"); len(metaO.Items) > 0 {
		for _, o := range metaO.Elem().Items {
			var m map[string]interface{}
			if err := hcl.DecodeObject(&m, o.Val); err != nil {
				return err
			}
			if err := mapstructure.WeakDecode(m, &result.Meta); err != nil {
				return err
			}
		}
	}

	return nil
}

// parseQuotaSpec is used to parse the quota specification from HCL
func ParseQuota(input []byte) (*api.QuotaSpec, error) {
	root, err := hcl.ParseBytes(input)
	if err != nil {
		return nil, err
	}

	// Top-level item should be a list
	list, ok := root.Node.(*ast.ObjectList)
	if !ok {
		return nil, fmt.Errorf("error parsing: root should be an object")
	}

	var spec api.QuotaSpec
	if err := parseQuotaSpecImpl(&spec, list); err != nil {
		return nil, err
	}

	return &spec, nil
}

// parseQuotaSpecImpl parses the quota spec taking as input the AST tree
func parseQuotaSpecImpl(result *api.QuotaSpec, list *ast.ObjectList) error {
	// Check for invalid keys
	valid := []string{
		"name",
		"description",
		"limit",
		"task_defaults",
		"task_max",
		"enforce",
	}
	if err := helper.CheckHCLKeys(list, valid); err != nil {
		return err
	}

	// Decode the full thing into a map[string]interface for ease
	var m map[string]interface{}
	if err := hcl.DecodeObject(&m, list); err != nil {
		return err
	}

	// Manually parse
	delete(m, "limit")
	delete(m, "task_defaults")
	delete(m, "task_max")

	// Decode the rest
	if err := mapstructure.WeakDecode(m, result); err != nil {
		return err
	}

	// Parse limits
	if o := list.Filter("limit"); len(o.Items) > 0 {
		if err := parseQuotaLimits(&result.Limits, o); err != nil {
			return multierror.Prefix(err, "limit ->")
		}
	}

	// Parse the task level defaults and maximums
	if o := list.Filter("task_defaults"); len(o.Items) > 0 {
		result.TaskDefaults = new(api.Resources)
		if err := parseQuotaResourceBlock("task_defaults", result.TaskDefaults, o, "cpu", "memory", "memory_max"); err != nil {
			return multierror.Prefix(err, "task_defaults ->")
		}
	}
	if o := list.Filter("task_max"); len(o.Items) > 0 {
		result.TaskMax = new(api.Resources)
		if err := parseQuotaResourceBlock("task_max", result.TaskMax, o, "cpu", "cores", "memory", "memory_max"); err != nil {
			return multierror.Prefix(err, "task_max ->")
		}
	}

	return nil
}

// parseQuotaLimits parses the quota limits
func parseQuotaLimits(result *[]*api.QuotaLimit, list *ast.ObjectList) error {
	for _, o := range list.Elem().Items {
		// Check for invalid keys
		valid := []string{
			"region",
			"region_limit",
			"variables_limit",
			"allocations",
		}
		if err := helper.CheckHCLKeys(o.Val, valid); err != nil {
			return err
		}

		var m map[string]interface{}
		if err := hcl.DecodeObject(&m, o.Val); err != nil {
			return err
		}

		// Manually parse
		delete(m, "region_limit")

		// Decode the rest
		var limit api.QuotaLimit
		if err := mapstructure.WeakDecode(m, &limit); err != nil {
			return err
		}

		// We need this later
		var listVal *ast.ObjectList
		if ot, ok := o.Val.(*ast.ObjectType); ok {
			listVal = ot.List
		} else {
			return fmt.Errorf("limit should be an object")
		}

		// Parse limits
		if o := listVal.Filter("region_limit"); len(o.Items) > 0 {
			limit.RegionLimit = new(api.Resources)
			if err := parseQuotaResource(limit.RegionLimit, o); err != nil {
				return multierror.Prefix(err, "region_limit ->")
			}
		}

		*result = append(*result, &limit)
	}

	return nil
}

// parseQuotaResource parses the region_limit resources
func parseQuotaResource(result *api.Resources, list *ast.ObjectList) error {
	return parseQuotaResourceBlock("region_limit", result, list, "cpu", "memory", "memory_max")
}

// parseQuotaResourceBlock parses a resources block of a quota specification,
// allowing only the given keys.
func parseQuotaResourceBlock(name string, result *api.Resources, list *ast.ObjectList, valid ...string) error {
	list = list.Elem()
	if len(list.Items) == 0 {
		return nil
	}
	if len(list.Items) > 1 {
		return fmt.Errorf("only one %q block allowed", name)
	}

	// Get our resource object
	o := list.Items[0]

	// We need this later
	var listVal *ast.ObjectList
	if ot, ok := o.Val.(*ast.ObjectType); ok {
		listVal = ot.List
	} else {
		return fmt.Errorf("resource: should be an object")
	}

	// Check for invalid keys
	if err := helper.CheckHCLKeys(listVal, valid); err != nil {
		return multierror.Prefix(err, "resources ->")
	}

	var m map[string]interface{}
	if err := hcl.DecodeObject(&m, o.Val); err != nil {
		return err
	}

	if err := mapstructure.WeakDecode(m, result); err != nil {
		return err
	}

	return nil
}

// ParseVariable is used to parse the variable specification
// from HCL
func ParseVariable(input []byte) (*api.Variable, error) {
	root, err := hcl.ParseBytes(input)
	if err != nil {
		return nil, err
	}

	// Top-level item should be a list
	list, ok := root.Node.(*ast.ObjectList)
	if !ok {
		return nil, fmt.Errorf("error parsing: root should be an object")
	}

	var out api.Variable
	if err := parseVariableSpecImpl(&out, list); err != nil {
		return nil, err
	}
	return &out, nil
}

// parseVariableSpecImpl parses the variable taking as input the AST tree
func parseVariableSpecImpl(result *api.Variable, list *ast.ObjectList) error {
	// Decode the full thing into a map[string]interface for ease
	var m map[string]interface{}
	if err := hcl.DecodeObject(&m, list); err != nil {
		return err
	}

	// Check for invalid keys
	valid := []string{
		"namespace",
		"path",
		"create_index",
		"modify_index",
		"create_time",
		"modify_time",
		"items",
	}
	if err := helper.CheckHCLKeys(list, valid); err != nil {
		return err
	}

	for _, index := range []string{"create_index", "modify_index"} {
		if value, ok := m[index]; ok {
			vInt, ok := value.(int)
			if !ok {
				return fmt.Errorf("%s must be integer; got (%T) %[2]v", index, value)
			}
			idx := uint64(vInt)
			n := strings.ReplaceAll(strings.Title(strings.ReplaceAll(index, "_", " ")), " ", "")
			m[n] = idx
			delete(m, index)
		}
	}

	for _, index := range []string{"create_time", "modify_time"} {
		if value, ok := m[index]; ok {
			vInt, ok := value.(int)
			if !ok {
				return fmt.Errorf("%s must be a int64; got a (%T) %[2]v", index, value)
			}
			n := strings.ReplaceAll(strings.Title(strings.ReplaceAll(index, "_", " ")), " ", "")
			m[n] = vInt
			delete(m, index)
		}
	}

	// Decode the rest
	if err := mapstructure.WeakDecode(m, result); err != nil {
		return err
	}

	return nil
}
