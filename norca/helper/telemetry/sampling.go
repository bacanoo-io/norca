// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"context"
	"net/http"
	"strings"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Span names of the client allocation lifecycle. Each phase of an
// allocation is a short trace of its own; they are always sampled.
const (
	SpanAllocStart   = "alloc.start"
	SpanAllocStop    = "alloc.stop"
	SpanAllocRestart = "alloc.restart"
	SpanAllocGC      = "alloc.gc"
)

// alwaysSampledRPC are the RPC methods whose new root traces are always
// sampled (decision 3 of the plan): the operations an operator wants to
// find again later. They are writes that change what runs or who may do
// what, and are rare compared to the reads, heartbeats and client syncs
// that make up most RPCs. A request that arrives with a parent follows the
// parent's decision instead, so the HTTP routes that lead to these methods
// are always sampled too (alwaysSampledHTTP).
var alwaysSampledRPC = setOf(
	// jobs
	"Job.Register", "Job.Deregister", "Job.BatchDeregister", "Job.Plan",
	"Job.Dispatch", "Job.Scale", "Job.Revert", "Job.Stable", "Job.Evaluate",
	"Periodic.Force",

	// operator endpoints: Raft, autopilot, scheduler configuration, seal,
	// keyring, backups and the Consul KV import
	"Operator.RaftGetConfiguration", "Operator.RaftRemovePeerByAddress",
	"Operator.RaftRemovePeerByID", "Operator.RaftPromoteStandby",
	"Operator.AutopilotGetConfiguration", "Operator.AutopilotSetConfiguration",
	"Operator.SchedulerGetConfiguration", "Operator.SchedulerSetConfiguration",
	"Operator.SealUnseal", "Operator.SealRecover", "Operator.SealRewrapDone",
	"Operator.SealRewrapConfirm", "Operator.ConsulKVMigrate", "Operator.ConsulKVRestore",
	"Keyring.Rotate", "Keyring.Update", "Keyring.Delete", "Keyring.Restore", "Keyring.Rewrap",
	"Backup.Save", "Backup.Restore", "Backup.ApproveRestore", "Backup.RejectRestore",
	"Backup.Update", "Backup.Delete", "Backup.TakeForLeader",
	"System.GarbageCollect", "System.ReconcileJobSummaries",

	// scaling decisions and actions: task groups (the autoscaler and
	// `job scale`), node groups and other custom objects
	"CustomObject.Scale", "CustomObject.Apply", "CustomObject.Delete",

	// clients: registration, drain, eligibility
	"Node.Register", "Node.Deregister", "Node.BatchDeregister",
	"Node.UpdateDrain", "Node.UpdateEligibility", "Node.Evaluate",

	// the allocation lifecycle: server side, and the client's own RPC
	// endpoints (service Allocations)
	"Alloc.Stop", "Alloc.Evict", "Alloc.UpdateDesiredTransition",
	"ClientAllocations.Restart", "ClientAllocations.Signal",
	"ClientAllocations.GarbageCollect", "ClientAllocations.GarbageCollectAll",
	"Allocations.Restart", "Allocations.Signal",
	"Allocations.GarbageCollect", "Allocations.GarbageCollectAll",

	// added by the endpoint review of phase 3: the other writes that change
	// what runs, where, or who may do what
	"Deployment.Promote", "Deployment.Pause", "Deployment.Fail", "Deployment.Cancel",
	"Deployment.Run", "Deployment.Unblock", "Deployment.SetAllocHealth",
	"Eval.Delete",
	"ACL.Bootstrap", "ACL.UpsertPolicies", "ACL.DeletePolicies", "ACL.UpsertTokens",
	"ACL.DeleteTokens", "ACL.UpsertRoles", "ACL.DeleteRolesByID",
	"ACL.UpsertAuthMethods", "ACL.DeleteAuthMethods",
	"ACL.UpsertBindingRules", "ACL.DeleteBindingRules",
	"Namespace.UpsertNamespaces", "Namespace.DeleteNamespaces",
	"NodePool.UpsertNodePools", "NodePool.DeleteNodePools",
	"Quota.UpsertQuotaSpecs", "Quota.DeleteQuotaSpecs",
	"AdmissionPolicy.UpsertAdmissionPolicies", "AdmissionPolicy.DeleteAdmissionPolicies",
	"NetworkPolicy.UpsertNetworkPolicies", "NetworkPolicy.DeleteNetworkPolicies",
	"NetworkPolicy.UpsertAdminNetworkPolicies", "NetworkPolicy.DeleteAdminNetworkPolicies",
	"ServiceIntention.Upsert", "ServiceIntention.Delete",
	"MTLS.RotateCA", "Certificate.ImportAuthority", "Certificate.RotateAuthority", "Certificate.Revoke",
	"ObjectKind.UpsertObjectKind", "ObjectKind.DeleteObjectKind", "ObjectKind.ApproveWebhook",
	"Release.Apply", "Release.Delete", "Release.Rollback", "Release.Test",
	"Rigger.Install", "Rigger.StackApply", "Rigger.StackDestroy", "Rigger.StackPlan",
	"Rigger.ImportBundle", "Rigger.DeleteBundle", "Rigger.CancelOperation", "Rigger.SyncRegistry",
	"CSIVolume.Register", "CSIVolume.Deregister", "CSIVolume.Create", "CSIVolume.Delete",
	"CSIVolume.CreateSnapshot", "CSIVolume.DeleteSnapshot", "CSIPlugin.Delete",
	"Secret.Upsert", "Secret.Delete", "Secret.Destroy", "Secret.Undelete",
	"SecretEngine.Upsert", "SecretEngine.Delete",
	"CDC.ConsumerUpsert", "CDC.ConsumerDelete",
)

// alwaysSampledSpans are the other span names whose root traces are always
// sampled: the phases of the allocation lifecycle on clients.
var alwaysSampledSpans = setOf(SpanAllocStart, SpanAllocStop, SpanAllocRestart, SpanAllocGC)

// httpMethods selects the methods of a route an HTTP rule applies to.
type httpMethods uint8

const (
	// writeMethods are PUT, POST and DELETE: the API's writes.
	writeMethods httpMethods = iota + 1

	// allMethods also includes the reads: the operator routes whose reads
	// are rare manual calls (the Raft configuration, the snapshot).
	allMethods
)

func (m httpMethods) match(method string) bool {
	switch m {
	case allMethods:
		return true
	case writeMethods:
		return method == http.MethodPut || method == http.MethodPost || method == http.MethodDelete
	}
	return false
}

// alwaysSampledHTTP maps the HTTP API routes (registered mux patterns) that
// lead to the always-sampled RPCs to the methods that do. The HTTP span is
// the root of an API request's trace, so the RPCs it calls follow its
// sampling decision.
var alwaysSampledHTTP = map[string]httpMethods{
	"/v1/jobs":                             writeMethods,
	"/v1/job/":                             writeMethods,
	"/v1/node/":                            writeMethods,
	"/v1/node/pools":                       writeMethods,
	"/v1/node/pool/":                       writeMethods,
	"/v1/allocation/":                      writeMethods,
	"/v1/client/allocation/":               writeMethods,
	"/v1/client/gc":                        writeMethods,
	"/v1/evaluations":                      writeMethods,
	"/v1/deployment/":                      writeMethods,
	"/v1/operator/raft/":                   allMethods,
	"/v1/operator/autopilot/configuration": allMethods,
	"/v1/operator/scheduler/configuration": allMethods,
	"/v1/operator/snapshot":                allMethods,
	"/v1/operator/keyring/":                writeMethods,
	"/v1/operator/unseal":                  writeMethods,
	"/v1/operator/seal/recover":            writeMethods,
	"/v1/operator/backups":                 writeMethods,
	"/v1/operator/backup/":                 writeMethods,
	"/v1/operator/consul-kv/migrate":       writeMethods,
	"/v1/operator/consul-kv/restore":       writeMethods,
	"/v1/system/gc":                        writeMethods,
	"/v1/system/reconcile/summaries":       writeMethods,
	"/v1/agent/join":                       writeMethods,
	"/v1/agent/force-leave":                writeMethods,
	"/v1/agent/keyring/":                   writeMethods,
	"/v1/agent/schedulers/config":          writeMethods,
	"/v1/acl/bootstrap":                    writeMethods,
	"/v1/acl/policy/":                      writeMethods,
	"/v1/acl/token":                        writeMethods,
	"/v1/acl/token/":                       writeMethods,
	"/v1/acl/role":                         writeMethods,
	"/v1/acl/role/":                        writeMethods,
	"/v1/acl/auth-method":                  writeMethods,
	"/v1/acl/auth-method/":                 writeMethods,
	"/v1/acl/binding-rule":                 writeMethods,
	"/v1/acl/binding-rule/":                writeMethods,
	"/v1/namespace":                        writeMethods,
	"/v1/namespace/":                       writeMethods,
	"/v1/quota":                            writeMethods,
	"/v1/quota/":                           writeMethods,
	"/v1/admission-policy/":                writeMethods,
	"/v1/network-policy/":                  writeMethods,
	"/v1/admin-network-policy/":            writeMethods,
	"/v1/service-intention":                writeMethods,
	"/v1/mtls/ca/rotate":                   writeMethods,
	"/v1/certificates/ca/import":           writeMethods,
	"/v1/certificates/ca/rotate":           writeMethods,
	"/v1/certificate/":                     writeMethods,
	"/v1/object-kind/":                     writeMethods,
	"/v1/object/":                          writeMethods, // but see objectSubresource
	"/v1/release/":                         writeMethods,
	"/v1/releases":                         writeMethods,
	"/v1/rigger/install":                   writeMethods,
	"/v1/rigger/stack/":                    writeMethods,
	"/v1/rigger/operation/":                writeMethods,
	"/v1/rigger/registry/":                 writeMethods,
	"/v1/rigger/bundle":                    writeMethods,
	"/v1/rigger/bundle/":                   writeMethods,
	"/v1/volume/csi/":                      writeMethods,
	"/v1/volumes/snapshot":                 writeMethods,
	"/v1/plugin/csi/":                      writeMethods,
	"/v1/secret/":                          writeMethods,
	"/v1/secret-engine/":                   writeMethods,
	"/v1/cdc/consumer/":                    writeMethods,
}

// objectSubresource reports whether a path of /v1/object/ names one of the
// subresources the operators write all the time (status, finalizers,
// events), which are not always sampled; the object itself and its scale
// subresource are.
func objectSubresource(path string) bool {
	rest := strings.TrimPrefix(path, "/v1/object/")
	if strings.Count(rest, "/") != 3 {
		return false
	}
	switch rest[strings.LastIndexByte(rest, '/')+1:] {
	case "status", "finalizers", "events":
		return true
	}
	return false
}

// alwaysSampleHTTP reports whether an HTTP API request is always sampled
// as a root.
func alwaysSampleHTTP(method, pattern, path string) bool {
	methods, ok := alwaysSampledHTTP[pattern]
	if !ok || !methods.match(method) {
		return false
	}
	if pattern == "/v1/object/" && objectSubresource(path) {
		return false
	}
	return true
}

// excludedRPC are the RPC methods that never get a span (on top of the
// blocking queries and the configured exclude_rpc): the heartbeats and the
// client's long polls and batches, which would dominate the traces and use
// up the sample ratio. Streaming RPCs never get one either: they are not
// served through the RPC codec.
var excludedRPC = []string{
	"Status.Ping",
	"Node.UpdateStatus",
	"Node.GetClientAllocs",
	"Node.UpdateAlloc",
	"Eval.Dequeue",
	// added by the endpoint review: the companion of Node.GetClientAllocs,
	// called by every client for the allocations that changed
	"Alloc.GetAllocs",
}

// excludedHTTP are the HTTP API routes (registered mux patterns) that never
// get a span: health checks and metrics scrapes, which are polled, the
// long-lived streams, and the web UI's files.
var excludedHTTP = setOf(
	"/v1/agent/health",
	"/v1/metrics",
	"/v1/metrics/history",
	"/v1/metrics/series",
	"/v1/metrics/summary",
	"/v1/event/stream",
	"/v1/agent/monitor",
	"/ui/",
	"/",
	"/debug/pprof/",
	"/debug/pprof/cmdline",
	"/debug/pprof/profile",
	"/debug/pprof/symbol",
	"/debug/pprof/trace",
)

// excludeHTTP reports whether an HTTP API request gets no span: an
// excluded route, a log or file stream, an exec session, or a blocking
// query (index > 0).
func excludeHTTP(r *http.Request, pattern string) bool {
	if _, ok := excludedHTTP[pattern]; ok {
		return true
	}
	path := r.URL.Path
	switch pattern {
	case "/v1/client/fs/":
		if strings.HasPrefix(path, "/v1/client/fs/logs/") || strings.HasPrefix(path, "/v1/client/fs/stream/") {
			return true
		}
	case "/v1/client/allocation/":
		if strings.HasSuffix(path, "/exec") {
			return true
		}
	}
	return blockingHTTPQuery(r)
}

// blockingHTTPQuery reports whether a request is a blocking query: an index
// parameter other than 0.
func blockingHTTPQuery(r *http.Request) bool {
	if r.URL.RawQuery == "" || !strings.Contains(r.URL.RawQuery, "index=") {
		return false
	}
	index := r.URL.Query().Get("index")
	return index != "" && index != "0"
}

// alwaysSampleKey marks a context whose new root span is always sampled.
type alwaysSampleKey struct{}

// withAlwaysSample marks ctx so that a root span started with it is
// sampled whatever the ratio.
func withAlwaysSample(ctx context.Context) context.Context {
	return context.WithValue(ctx, alwaysSampleKey{}, true)
}

// rootSampler decides the new root traces: always for the always-sampled
// RPC methods, span names and marked HTTP requests, otherwise at the ratio.
type rootSampler struct {
	ratio sdktrace.Sampler
}

var _ sdktrace.Sampler = (*rootSampler)(nil)

func (s *rootSampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	if alwaysSampled(p) {
		return sdktrace.SamplingResult{
			Decision:   sdktrace.RecordAndSample,
			Tracestate: trace.SpanContextFromContext(p.ParentContext).TraceState(),
		}
	}
	return s.ratio.ShouldSample(p)
}

func alwaysSampled(p sdktrace.SamplingParameters) bool {
	if _, ok := alwaysSampledRPC[p.Name]; ok {
		return true
	}
	if _, ok := alwaysSampledSpans[p.Name]; ok {
		return true
	}
	return p.ParentContext != nil && p.ParentContext.Value(alwaysSampleKey{}) != nil
}

func (s *rootSampler) Description() string {
	return "NorcaRoot{always-sampled operations," + s.ratio.Description() + "}"
}

// NewSampler returns the sampler of the agent's traces: a sampled or
// unsampled parent decides for its children (internal RPC parents are
// remote and honoured); a new root is sampled always for the important
// operations and otherwise at the given ratio sampler (for a fixed ratio,
// sdktrace.TraceIDRatioBased).
func NewSampler(ratio sdktrace.Sampler) sdktrace.Sampler {
	return sdktrace.ParentBased(&rootSampler{ratio: ratio})
}

// AlwaysSampledRPC reports whether new root traces of an RPC method are
// always sampled.
func AlwaysSampledRPC(method string) bool {
	_, ok := alwaysSampledRPC[method]
	return ok
}

func setOf(values ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(values))
	for _, v := range values {
		m[v] = struct{}{}
	}
	return m
}
