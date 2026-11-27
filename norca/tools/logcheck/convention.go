// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package logcheck

// The key convention of the agent's logs. The identity keys below are the
// ones every component uses for the same thing; any other key is a lower
// snake_case word or phrase that names what its value is at that call site.
//
//	error                        the error (key ending in _error for a second one)
//	namespace                    namespace name
//	job_id, job_version          job ID, job version
//	task_group, task             task group name, task name
//	alloc_id, alloc_name         allocation ID, allocation name
//	eval_id                      evaluation ID
//	deployment_id                deployment ID
//	node_id, node_name           client node ID, client node name
//	server_id, server, server_addr  server ID, server name, server address
//	region, datacenter, node_pool
//	rpc_method                   RPC method, Service.Method
//	http_method, http_path, http_status  HTTP request
//	duration                     a time.Duration
//	index                        a Raft or state store index
//	address                      a network address
//	driver, plugin, plugin_type  plugin name and type
//	volume_id                    volume ID
//	object_kind, object_name     custom objects
//	trace_id, span_id            set by helper/telemetry only
//
// An object is never logged as a value: its ID is (objectTypes).

// renamed maps a key that is never right to the key of the convention that
// replaces it. The analyzer offers the replacement as a fix.
var renamed = map[string]string{
	"err":            "error",
	"create_err":     "create_error",
	"ns":             "namespace",
	"job":            "job_id",
	"tg":             "task_group",
	"task_name":      "task",
	"alloc":          "alloc_id",
	"aid":            "alloc_id",
	"previous_alloc": "previous_alloc_id",
	"eval":           "eval_id",
	"deployment":     "deployment_id",
	"lookup_node_id": "node_id",
	"peer_id":        "server_id",
	"peer_name":      "server",
	"peer_address":   "server_addr",
	"dc":             "datacenter",
	"dcs":            "datacenters",
	"took":           "duration",
	"elapsed":        "duration",
	"raft_index":     "index",
	"addr":           "address",
	"msg":            "message",
	"vol":            "volume_id",
	"plugin_name":    "plugin",
	"rpc":            "rpc_method",
	"stack":          "stack_trace",
	"requsted":       "requested",
}

// ambiguous lists keys that stand for more than one key of the convention;
// the call site decides which one it means.
var ambiguous = map[string]string{
	"id":     "name what the ID identifies (alloc_id, backup_id, server_id, ...)",
	"node":   "use node_id or node_name",
	"group":  "use task_group (or node_group for a node group)",
	"peer":   "use server, server_id or server_addr",
	"method": "use rpc_method or http_method",
	"object": "use object_kind and object_name",
	"time":   "use duration for a time.Duration, or name the point in time",
}

// reserved lists keys that only the package allowed for them sets.
var reserved = map[string]string{
	"trace_id": "helper/telemetry",
	"span_id":  "helper/telemetry",
}

// structsPath is the import path of the package of the server's structs.
const structsPath = "github.com/bacanoo-io/norca/norca/norca/structs"

// objectKey says how an object of the structs package is logged: by the
// value of one of its fields under a key of the convention.
type objectKey struct {
	key   string
	field string
}

// objectTypes lists the types of the structs package that are never logged
// as a whole (best effort: the objects with an identity of their own).
var objectTypes = map[string]objectKey{
	"Allocation":            {key: "alloc_id", field: "ID"},
	"AllocListStub":         {key: "alloc_id", field: "ID"},
	"Job":                   {key: "job_id", field: "ID"},
	"JobListStub":           {key: "job_id", field: "ID"},
	"Evaluation":            {key: "eval_id", field: "ID"},
	"Deployment":            {key: "deployment_id", field: "ID"},
	"Node":                  {key: "node_id", field: "ID"},
	"NodeListStub":          {key: "node_id", field: "ID"},
	"CSIVolume":             {key: "volume_id", field: "ID"},
	"HostVolume":            {key: "volume_id", field: "ID"},
	"TaskGroup":             {key: "task_group", field: "Name"},
	"Task":                  {key: "task", field: "Name"},
	"ACLToken":              {key: "accessor_id", field: "AccessorID"},
	"NamespacedID":          {key: "job_id", field: "ID"},
	"AuthenticatedIdentity": {key: "identity", field: "String()"},
}

// secretFields lists field names whose values are secrets and never logged.
var secretFields = map[string]bool{
	"SecretID":  true,
	"AuthToken": true,
}
