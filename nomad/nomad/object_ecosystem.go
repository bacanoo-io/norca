// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	metrics "github.com/armon/go-metrics"
	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/admission"
	"github.com/hashicorp/nomad/nomad/objectschema"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
	"golang.org/x/time/rate"
)

// The ecosystem of the operator framework (phase 4) on the server:
// type-bound webhooks, schema versions with migration CEL, and load
// management of the objects of a kind. See objects_ecosystem.go in structs.

const (
	// objectOperationDelete and objectOperationMigrate are the operations
	// CEL rules see in request.operation for reviews of deletions and for
	// the migration check of stored objects.
	objectOperationDelete  = "delete"
	objectOperationMigrate = "migrate"

	// maxIdentityLimiters bounds the per-identity limiters of a kind; idle
	// ones are dropped beyond it.
	maxIdentityLimiters = 4096

	// identityLimiterIdle is how long an identity limiter is kept unused.
	identityLimiterIdle = 10 * time.Minute

	// objectMigrationInterval is how often the leader looks for objects to
	// migrate; objectMigrationBatch how many it migrates per round, and
	// objectMigrationPause the pause between two, so that a migration
	// doesn't flood Raft.
	objectMigrationInterval = 5 * time.Second
	objectMigrationBatch    = 200
	objectMigrationPause    = 10 * time.Millisecond
)

// objectEcosystem is the server state of the ecosystem features: clients of
// type-bound webhooks, compiled migrations, rate limiters and watch
// counters.
type objectEcosystem struct {
	migrations *admission.MigrationEngine

	lock     sync.Mutex
	webhooks map[string]*cachedObjectWebhook
	limiters map[string]*objectKindLimiter
	watches  map[string]int
}

type cachedObjectWebhook struct {
	digest string
	client *admission.Webhook
}

// objectKindLimiter holds the token buckets of a kind, for its limits at a
// modify index.
type objectKindLimiter struct {
	modifyIndex uint64
	all         *rate.Limiter
	identities  map[string]*identityLimiter
}

type identityLimiter struct {
	limiter  *rate.Limiter
	lastUsed time.Time
}

func newObjectEcosystem() (*objectEcosystem, error) {
	migrations, err := admission.NewMigrationEngine()
	if err != nil {
		return nil, err
	}
	return &objectEcosystem{
		migrations: migrations,
		webhooks:   make(map[string]*cachedObjectWebhook),
		limiters:   make(map[string]*objectKindLimiter),
		watches:    make(map[string]int),
	}, nil
}

// forget drops the cached state of a kind.
func (e *objectEcosystem) forget(kindRef string) {
	e.lock.Lock()
	for key, w := range e.webhooks {
		if strings.HasPrefix(key, kindRef+"\x00") {
			w.client.Close()
			delete(e.webhooks, key)
		}
	}
	delete(e.limiters, kindRef)
	e.lock.Unlock()
	e.migrations.Forget(kindRef)
}

// webhookClient returns the client of a type-bound webhook, built once per
// configuration.
func (e *objectEcosystem) webhookClient(kind *structs.ObjectKind, w *structs.ObjectKindWebhook) (*admission.Webhook, error) {
	key := kind.KindRef() + "\x00" + w.Name
	digest := w.Digest()
	e.lock.Lock()
	defer e.lock.Unlock()
	if cached, ok := e.webhooks[key]; ok {
		if cached.digest == digest {
			return cached.client, nil
		}
		cached.client.Close()
		delete(e.webhooks, key)
	}
	client, err := admission.NewObjectWebhook(kind.KindRef(), w)
	if err != nil {
		return nil, err
	}
	e.webhooks[key] = &cachedObjectWebhook{digest: digest, client: client}
	return client, nil
}

// objectIdentity returns the key of the per-identity limits of a request:
// the token's accessor ID, the workload identity or "anonymous".
func objectIdentity(args structs.RequestWithIdentity) string {
	user := admissionUser(args)
	switch {
	case user.AccessorID != "":
		return "token:" + user.AccessorID
	case user.Name != "":
		return user.Name
	}
	return "anonymous"
}

// allowWrite takes a token from the write buckets of the kind and of the
// identity. It fails with 429 when a bucket is empty.
func (e *objectEcosystem) allowWrite(kind *structs.ObjectKind, identity string) error {
	l := kind.Limits
	if l == nil || (l.WriteRate <= 0 && l.IdentityWriteRate <= 0) {
		return nil
	}
	now := time.Now()
	ref := kind.KindRef()

	e.lock.Lock()
	kl, ok := e.limiters[ref]
	if !ok || kl.modifyIndex != kind.ModifyIndex {
		kl = &objectKindLimiter{modifyIndex: kind.ModifyIndex, identities: map[string]*identityLimiter{}}
		if l.WriteRate > 0 {
			kl.all = rate.NewLimiter(rate.Limit(l.WriteRate), max(1, l.WriteBurst))
		}
		e.limiters[ref] = kl
	}
	var il *identityLimiter
	if l.IdentityWriteRate > 0 {
		il = kl.identities[identity]
		if il == nil {
			if len(kl.identities) >= maxIdentityLimiters {
				for id, other := range kl.identities {
					if now.Sub(other.lastUsed) > identityLimiterIdle {
						delete(kl.identities, id)
					}
				}
			}
			il = &identityLimiter{limiter: rate.NewLimiter(rate.Limit(l.IdentityWriteRate), max(1, l.IdentityWriteBurst))}
			kl.identities[identity] = il
		}
		il.lastUsed = now
	}
	e.lock.Unlock()

	// The identity bucket is checked first, so that a misbehaving identity
	// is throttled before it consumes the kind's budget.
	if il != nil && !il.limiter.AllowN(now, 1) {
		metrics.IncrCounterWithLabels([]string{"nomad", "custom_object", "throttled"}, 1,
			[]metrics.Label{{Name: "kind", Value: ref}, {Name: "limit", Value: "identity_write_rate"}})
		return structs.NewObjectLimitError("writes of %s by %s exceed %g per second", ref, identity, l.IdentityWriteRate)
	}
	if kl.all != nil && !kl.all.AllowN(now, 1) {
		metrics.IncrCounterWithLabels([]string{"nomad", "custom_object", "throttled"}, 1,
			[]metrics.Label{{Name: "kind", Value: ref}, {Name: "limit", Value: "write_rate"}})
		return structs.NewObjectLimitError("writes of %s exceed %g per second", ref, l.WriteRate)
	}
	return nil
}

// acquireWatch counts a blocking query of an identity on objects of the
// kind. The returned function releases it.
func (e *objectEcosystem) acquireWatch(kind *structs.ObjectKind, identity string) (func(), error) {
	if kind == nil || kind.Limits == nil || kind.Limits.MaxWatchesPerIdentity <= 0 {
		return func() {}, nil
	}
	key := kind.KindRef() + "\x00" + identity
	e.lock.Lock()
	defer e.lock.Unlock()
	if e.watches[key] >= kind.Limits.MaxWatchesPerIdentity {
		metrics.IncrCounterWithLabels([]string{"nomad", "custom_object", "throttled"}, 1,
			[]metrics.Label{{Name: "kind", Value: kind.KindRef()}, {Name: "limit", Value: "max_watches_per_identity"}})
		return nil, structs.NewObjectLimitError("%s has %d watches of %s open, the limit",
			identity, e.watches[key], kind.KindRef())
	}
	e.watches[key]++
	var once sync.Once
	return func() {
		once.Do(func() {
			e.lock.Lock()
			defer e.lock.Unlock()
			if e.watches[key] <= 1 {
				delete(e.watches, key)
			} else {
				e.watches[key]--
			}
		})
	}, nil
}

// checkObjectWriteLimits applies the write rate limits of the kind to a
// request.
func (s *Server) checkObjectWriteLimits(kind *structs.ObjectKind, args structs.RequestWithIdentity) error {
	return s.objects.eco.allowWrite(kind, objectIdentity(args))
}

// objectWatch starts a blocking query on objects of a kind, applying the
// watch limit of the kind. Queries that don't block are not counted.
func (s *Server) objectWatch(group, kindName string, q *structs.QueryOptions, args structs.RequestWithIdentity) (func(), error) {
	if q.MinQueryIndex == 0 {
		return func() {}, nil
	}
	kind, err := s.State().ObjectKindByName(nil, group, kindName)
	if err != nil || kind == nil {
		return func() {}, err
	}
	return s.objects.eco.acquireWatch(kind, objectIdentity(args))
}

// checkObjectCount enforces the per-namespace object limit of the kind on
// the creation of an object.
func checkObjectCount(snap *state.StateSnapshot, kind *structs.ObjectKind, namespace string) error {
	if kind.Limits == nil || kind.Limits.MaxObjectsPerNamespace <= 0 {
		return nil
	}
	iter, err := snap.CustomObjects(nil, kind.Group, kind.Kind, namespace, state.SortDefault)
	if err != nil {
		return err
	}
	n := 0
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		if n++; n >= kind.Limits.MaxObjectsPerNamespace {
			where := fmt.Sprintf("namespace %q", namespace)
			if !kind.Namespaced {
				where = "the cluster"
			}
			return structs.NewObjectLimitError("%s has %d objects of %s, the limit", where, n, kind.KindRef())
		}
	}
	return nil
}

// objectRuleRequest returns the request variable of CEL rules.
func objectRuleRequest(operation, namespace string, dryRun bool, user *structs.AdmissionUser) map[string]any {
	return map[string]any{
		"operation": operation,
		"namespace": namespace,
		"dryRun":    dryRun,
		"user":      admissionUserValue(user),
	}
}

// migrateObject converts an object to the schema version of the kind with
// the prepared migration, fills in the defaults of the kind's schema and
// validates the result. It returns the migrated copy (KindVersion set) and
// whether its spec or status changed. A nil schema skips the defaults and
// the validation.
func migrateObject(prepared *admission.PreparedMigration, schema *objectschema.Schema, kind *structs.ObjectKind,
	obj *structs.CustomObject) (*structs.CustomObject, bool, error) {

	out := obj.Copy()
	out.KindVersion = kind.SchemaVersion()
	if prepared != nil && obj.SchemaVersion() < kind.SchemaVersion() {
		self, err := admission.ObjectValue(objectCELValue(obj))
		if err != nil {
			return nil, false, err
		}
		spec, status, err := prepared.Migrate(context.Background(), self, obj.SchemaVersion(),
			structs.CopyObjectMap(obj.Spec), structs.CopyObjectMap(obj.Status))
		if err != nil {
			return nil, false, err
		}
		if out.Spec, err = structs.NormalizeObjectMap(spec); err != nil {
			return nil, false, fmt.Errorf("migrated spec can't be encoded as JSON: %v", err)
		}
		if out.Status, err = structs.NormalizeObjectMap(status); err != nil {
			return nil, false, fmt.Errorf("migrated status can't be encoded as JSON: %v", err)
		}
		if len(out.Status) == 0 {
			out.Status = nil
		}
	}
	if schema != nil {
		doc := map[string]any{"spec": out.Spec}
		if out.Spec == nil {
			doc["spec"] = map[string]any{}
		}
		if out.Status != nil {
			doc["status"] = out.Status
		}
		schema.ApplyDefaults(doc)
		out.Spec, _ = doc["spec"].(map[string]any)
		if st, ok := doc["status"].(map[string]any); ok {
			out.Status = st
		}
		if err := schema.Validate(objectDocument(out)); err != nil {
			return nil, false, err
		}
	}
	changed := !structs.ObjectMapsEqual(obj.Spec, out.Spec) || !structs.ObjectMapsEqual(obj.Status, out.Status)
	if !structs.ObjectMapsEqual(obj.Spec, out.Spec) {
		out.Generation++
	}
	return out, changed, nil
}

// migrateOnWrite returns the stored object converted to the schema version
// of its kind, so that writes of objects the background migration hasn't
// reached yet see (and keep) the current format. Objects at the current
// version, and kinds without a migration, are returned as they are.
func (s *Server) migrateOnWrite(kind *structs.ObjectKind, existing *structs.CustomObject) (*structs.CustomObject, error) {
	if existing == nil || kind.Migration == nil || existing.SchemaVersion() >= kind.SchemaVersion() {
		return existing, nil
	}
	prepared, err := s.objects.eco.migrations.ForKind(kind)
	if err != nil {
		return nil, fmt.Errorf("object kind %q has an invalid migration: %v", kind.KindRef(), err)
	}
	schema, err := s.objects.schema(kind)
	if err != nil {
		return nil, fmt.Errorf("object kind %q has an invalid schema: %v", kind.KindRef(), err)
	}
	migrated, _, err := migrateObject(prepared, schema, kind, existing)
	if err != nil {
		return nil, structs.NewErrRPCCodedf(http.StatusBadRequest,
			"%s %q can't be migrated to schema version %d: %v", kind.KindRef(), existing.Name, kind.SchemaVersion(), err)
	}
	return migrated, nil
}

// objectReview describes a write of an object for the type-bound webhooks.
type objectReview struct {
	kind      *structs.ObjectKind
	operation string
	obj       *structs.CustomObject
	existing  *structs.CustomObject
	args      structs.RequestWithIdentity
	dryRun    bool
}

// webhooks returns the webhooks of the kind of the given mode that review
// the operation.
func (r *objectReview) webhooks(mode string) []*structs.ObjectKindWebhook {
	var out []*structs.ObjectKindWebhook
	for _, w := range r.kind.Webhooks {
		if w == nil {
			continue
		}
		m := w.Mode
		if m == "" {
			m = structs.ObjectWebhookModeValidate
		}
		if m == mode && w.Reviews(r.operation) {
			out = append(out, w)
		}
	}
	return out
}

// objectWebhookFailure applies the failure policy of a type-bound webhook
// to a failure: a warning for "ignore", an error otherwise.
func (s *Server) objectWebhookFailure(kind *structs.ObjectKind, w *structs.ObjectKindWebhook, err error) error {
	labels := []metrics.Label{{Name: "kind", Value: kind.KindRef()}, {Name: "webhook", Value: w.Name}}
	metrics.IncrCounterWithLabels([]string{"nomad", "custom_object", "webhook", "failures"}, 1, labels)
	if w.IgnoreFailure() {
		s.logger.Warn("object webhook failed, ignoring", "kind", kind.KindRef(), "webhook", w.Name, "error", err)
		return &admissionWarning{fmt.Errorf("webhook %q of %s failed and was ignored: %v", w.Name, kind.KindRef(), err)}
	}
	s.logger.Warn("object webhook failed, rejecting the write", "kind", kind.KindRef(), "webhook", w.Name, "error", err)
	return structs.NewErrRPCCodedf(http.StatusForbidden, "webhook %q of %s failed: %v", w.Name, kind.KindRef(), err)
}

// callObjectWebhook sends the review to a webhook. Webhooks that are not
// approved are not called; they count as failed.
func (s *Server) callObjectWebhook(r *objectReview, w *structs.ObjectKindWebhook) (*admission.ReviewResponse, error) {
	if !w.Approved() {
		return nil, s.objectWebhookFailure(r.kind, w, fmt.Errorf("the webhook is not approved by an administrator"))
	}
	client, err := s.objects.eco.webhookClient(r.kind, w)
	if err != nil {
		return nil, s.objectWebhookFailure(r.kind, w, err)
	}
	req := &admission.ReviewRequest{
		UID:        uuid.Generate(),
		Resource:   structs.AdmissionResourceObject,
		Operation:  r.operation,
		DryRun:     r.dryRun,
		Namespace:  r.obj.Namespace,
		User:       admission.NewReviewUser(admissionUser(r.args)),
		ObjectKind: r.kind.KindRef(),
	}
	if req.Object, err = reviewObject(r.obj); err != nil {
		return nil, err
	}
	if r.existing != nil {
		if req.OldObject, err = reviewObject(r.existing); err != nil {
			return nil, err
		}
	}
	resp, err := client.Review(s.shutdownCtx, req)
	if err != nil {
		return nil, s.objectWebhookFailure(r.kind, w, err)
	}
	return resp, nil
}

// reviewObject returns the wire form of an object in a review.
func reviewObject(obj *structs.CustomObject) (map[string]any, error) {
	b, err := json.Marshal(objectCELValue(obj))
	if err != nil {
		return nil, err
	}
	var out map[string]any
	return out, json.Unmarshal(b, &out)
}

// objectWebhookDenied returns the error of a write a webhook rejected.
func objectWebhookDenied(kind *structs.ObjectKind, w *structs.ObjectKindWebhook, resp *admission.ReviewResponse) error {
	msg := resp.Message
	if msg == "" {
		msg = "no reason given"
	}
	return structs.NewErrRPCCodedf(http.StatusBadRequest, "webhook %q of %s denied the write: %s", w.Name, kind.KindRef(), msg)
}

// runDefaultingWebhooks calls the defaulting webhooks of the kind in order;
// each sees the spec the previous one patched. Patches may only change the
// spec. It returns the warnings.
func (s *Server) runDefaultingWebhooks(r *objectReview) ([]string, error) {
	var warnings []string
	for _, w := range r.webhooks(structs.ObjectWebhookModeDefault) {
		resp, err := s.callObjectWebhook(r, w)
		if err != nil {
			warning, failure := splitWarning(err)
			if failure != nil {
				return warnings, failure
			}
			warnings = append(warnings, warning.Error())
			continue
		}
		if !resp.Allowed {
			return warnings, objectWebhookDenied(r.kind, w, resp)
		}
		for _, msg := range resp.Warnings {
			warnings = append(warnings, fmt.Sprintf("webhook %q: %s", w.Name, msg))
		}
		spec, err := patchObjectSpec(r.obj, resp)
		if err != nil {
			warning, failure := splitWarning(s.objectWebhookFailure(r.kind, w, err))
			if failure != nil {
				return warnings, failure
			}
			warnings = append(warnings, warning.Error())
			continue
		}
		if spec != nil {
			r.obj.Spec = spec
		}
	}
	return warnings, nil
}

// patchObjectSpec applies the patch of a defaulting webhook to the review
// form of the object and returns the patched spec, or nil without a patch.
// Changes outside the spec are rejected.
func patchObjectSpec(obj *structs.CustomObject, resp *admission.ReviewResponse) (map[string]any, error) {
	patch, err := resp.PatchOperations()
	if err != nil || patch == nil {
		return nil, err
	}
	before, err := reviewObject(obj)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(before)
	if err != nil {
		return nil, err
	}
	raw, err = admission.ApplyJSONPatch(raw, patch)
	if err != nil {
		return nil, err
	}
	var after map[string]any
	if err := json.Unmarshal(raw, &after); err != nil {
		return nil, err
	}
	for k, v := range after {
		if k == "spec" {
			continue
		}
		if !reflect.DeepEqual(before[k], v) {
			return nil, fmt.Errorf("patch may only change the spec, not %q", k)
		}
	}
	for k := range before {
		if _, ok := after[k]; !ok {
			return nil, fmt.Errorf("patch may only change the spec, not %q", k)
		}
	}
	spec, ok := after["spec"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("patched spec is not an object")
	}
	return structs.NormalizeObjectMap(spec)
}

// runValidatingWebhooks calls the validating webhooks of the kind in order.
// It returns the warnings.
func (s *Server) runValidatingWebhooks(r *objectReview) ([]string, error) {
	var warnings []string
	for _, w := range r.webhooks(structs.ObjectWebhookModeValidate) {
		resp, err := s.callObjectWebhook(r, w)
		if err != nil {
			warning, failure := splitWarning(err)
			if failure != nil {
				return warnings, failure
			}
			warnings = append(warnings, warning.Error())
			continue
		}
		if !resp.Allowed {
			return warnings, objectWebhookDenied(r.kind, w, resp)
		}
		for _, msg := range resp.Warnings {
			warnings = append(warnings, fmt.Sprintf("webhook %q: %s", w.Name, msg))
		}
		if patch, _ := resp.PatchOperations(); patch != nil {
			warnings = append(warnings, fmt.Sprintf("webhook %q returned a patch, which is ignored for validating webhooks", w.Name))
		}
	}
	return warnings, nil
}

// prepareObjectKindUpsert completes an upsert of a kind on the leader: it
// carries the approvals of unchanged webhooks over from the stored kind,
// approves the webhooks of kinds written by management tokens, rejects a
// lower schema version and checks the stored objects of the kind against the
// new schema (after the migration, if the version is raised). It returns the
// migration report (nil if the objects were not checked) and the warnings.
func (k *ObjectKind) prepareObjectKindUpsert(args *structs.ObjectKindUpsertRequest, aclObj *acl.ACL, kind, existing *structs.ObjectKind,
	snap *state.StateSnapshot) (*structs.ObjectKindMigrationReport, []string, error) {

	var warnings []string
	if existing != nil && kind.SchemaVersion() < existing.SchemaVersion() {
		return nil, nil, structs.NewErrRPCCodedf(http.StatusBadRequest,
			"object kind %q: version %d is lower than the stored version %d", kind.KindRef(), kind.SchemaVersion(), existing.SchemaVersion())
	}
	if err := k.srv.objects.eco.migrations.Compile(kind.Migration); err != nil {
		return nil, nil, structs.NewErrRPCCodedf(http.StatusBadRequest, "invalid object kind %q: migration: %v", kind.KindRef(), err)
	}

	// Approvals: never taken from the request.
	management := aclObj == nil || aclObj.IsManagement()
	user := admissionUser(args)
	for _, w := range kind.Webhooks {
		w.Approval = nil
		if existing != nil {
			if old := existing.Webhook(w.Name); old != nil && old.Approved() && old.Approval.Digest == w.Digest() {
				a := *old.Approval
				w.Approval = &a
				continue
			}
		}
		if management {
			w.Approval = newWebhookApproval(w, user)
			continue
		}
		effect := fmt.Sprintf("writes of %s it reviews fail", kind.KindRef())
		if w.IgnoreFailure() {
			effect = "it is skipped (failure policy \"ignore\")"
		}
		warnings = append(warnings, fmt.Sprintf(
			"webhook %q is pending approval by an administrator (norca object-kind webhook approve %s %s); until then %s",
			w.Name, kind.KindRef(), w.Name, effect))
	}

	if existing == nil || !objectKindSchemaChanged(existing, kind) {
		return nil, warnings, nil
	}
	report, ruleWarnings, err := k.checkStoredObjects(snap, kind, existing, user)
	if err != nil {
		return nil, nil, err
	}
	warnings = append(warnings, ruleWarnings...)
	return report, warnings, nil
}

// newWebhookApproval returns an approval of the webhook by the user.
func newWebhookApproval(w *structs.ObjectKindWebhook, user *structs.AdmissionUser) *structs.ObjectKindWebhookApproval {
	by := "management"
	switch {
	case user.AccessorID != "":
		by = "token:" + user.AccessorID
		if user.Name != "" {
			by += " (" + user.Name + ")"
		}
	case user.Type == "anonymous":
		by = "anonymous (ACLs disabled)"
	}
	return &structs.ObjectKindWebhookApproval{Digest: w.Digest(), ApprovedBy: by, ApproveTime: time.Now().UnixNano()}
}

// objectKindSchemaChanged reports whether a kind write changes what the
// stored objects are validated against.
func objectKindSchemaChanged(existing, kind *structs.ObjectKind) bool {
	return existing.Schema != kind.Schema ||
		existing.SchemaVersion() != kind.SchemaVersion() ||
		!reflect.DeepEqual(existing.Migration, kind.Migration) ||
		!reflect.DeepEqual(existing.Rules, kind.Rules)
}

// checkStoredObjects runs the migration (for objects at an older schema
// version) and the new schema on every stored object of the kind. Objects
// that would be invalid are failures; violations of the new CEL rules are
// returned as warnings, since they don't block the controllers' status
// writes of the object if the rules allow the "status" operation.
func (k *ObjectKind) checkStoredObjects(snap *state.StateSnapshot, kind, existing *structs.ObjectKind,
	user *structs.AdmissionUser) (*structs.ObjectKindMigrationReport, []string, error) {

	schema, err := objectschema.Compile(kind.Group, kind.Kind, kind.Schema)
	if err != nil {
		return nil, nil, structs.NewErrRPCCodedf(http.StatusBadRequest, "invalid object kind %q: schema: %v", kind.KindRef(), err)
	}
	prepared, err := k.srv.objects.eco.migrations.Prepare(kind.Migration, kind.SchemaVersion())
	if err != nil {
		return nil, nil, structs.NewErrRPCCodedf(http.StatusBadRequest, "invalid object kind %q: migration: %v", kind.KindRef(), err)
	}
	rules := k.srv.objects.rules.Prepare(kind)

	iter, err := snap.CustomObjects(nil, kind.Group, kind.Kind, structs.AllNamespacesSentinel, state.SortDefault)
	if err != nil {
		return nil, nil, err
	}
	report := &structs.ObjectKindMigrationReport{FromVersion: existing.SchemaVersion(), ToVersion: kind.SchemaVersion()}
	var warnings []string
	ruleViolations := 0
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		obj := raw.(*structs.CustomObject)
		report.Objects++
		if obj.SchemaVersion() < kind.SchemaVersion() {
			report.Pending++
		}
		migrated, changed, err := migrateObject(prepared, schema, kind, obj)
		if err != nil {
			report.Failed++
			if len(report.Failures) < structs.MaxObjectMigrationFailures {
				report.Failures = append(report.Failures, &structs.ObjectMigrationFailure{
					Namespace: obj.Namespace, Name: obj.Name, Error: err.Error(),
				})
			}
			continue
		}
		if changed {
			report.Changed++
			if len(report.Samples) < structs.MaxObjectMigrationSamples {
				report.Samples = append(report.Samples, &structs.ObjectMigrationSample{
					Namespace: obj.Namespace, Name: obj.Name,
					SpecBefore: obj.Spec, SpecAfter: migrated.Spec,
					StatusBefore: obj.Status, StatusAfter: migrated.Status,
				})
			}
		}
		if len(kind.Rules) > 0 {
			self, err := admission.ObjectValue(objectCELValue(migrated))
			if err != nil {
				return nil, nil, err
			}
			oldSelf, err := admission.ObjectValue(objectCELValue(obj))
			if err != nil {
				return nil, nil, err
			}
			violations := rules.Evaluate(context.Background(), self, oldSelf,
				objectRuleRequest(objectOperationMigrate, obj.Namespace, true, user))
			if len(violations) > 0 {
				ruleViolations++
				if ruleViolations <= 5 {
					warnings = append(warnings, fmt.Sprintf("%s %q violates a rule of the new kind: %s",
						kind.KindRef(), objectDisplayName(obj), violations[0].Message))
				}
			}
		}
	}
	if ruleViolations > 5 {
		warnings = append(warnings, fmt.Sprintf("%d more objects violate rules of the new kind", ruleViolations-5))
	}
	return report, warnings, nil
}

func objectDisplayName(obj *structs.CustomObject) string {
	if obj.Namespace == "" {
		return obj.Name
	}
	return obj.Namespace + "/" + obj.Name
}

// migrationReportError returns the error rejecting a kind write that would
// make stored objects invalid.
func migrationReportError(kind *structs.ObjectKind, report *structs.ObjectKindMigrationReport) error {
	msgs := make([]string, 0, len(report.Failures))
	for _, f := range report.Failures {
		name := f.Name
		if f.Namespace != "" {
			name = f.Namespace + "/" + f.Name
		}
		msgs = append(msgs, fmt.Sprintf("%s: %s", name, f.Error))
	}
	more := ""
	if report.Failed > len(report.Failures) {
		more = fmt.Sprintf(" (and %d more)", report.Failed-len(report.Failures))
	}
	hint := "raise the version and add a migration that converts them"
	if kind.Migration != nil {
		hint = "fix the migration"
	}
	return structs.NewErrRPCCodedf(http.StatusConflict,
		"object kind %q would make %d stored objects invalid; %s (norca object-kind apply -dry-run shows the report): %s%s",
		kind.KindRef(), report.Failed, hint, strings.Join(msgs, "; "), more)
}

// ApproveWebhook approves a type-bound webhook of a kind, or revokes its
// approval. It requires a management token. The approval covers the
// current configuration of the webhook; any change of it needs a new one.
func (k *ObjectKind) ApproveWebhook(args *structs.ObjectKindWebhookApprovalRequest, reply *structs.ObjectKindUpsertResponse) error {
	authErr := k.srv.Authenticate(k.ctx, args)
	if done, err := k.srv.forward("ObjectKind.ApproveWebhook", args, args, reply); done {
		return err
	}
	k.srv.MeasureRPCRate("object_kind", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "object_kind", "approve_webhook"}, time.Now())

	aclObj, err := k.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	if aclObj != nil && !aclObj.IsManagement() {
		return structs.ErrPermissionDenied
	}
	if !ServersMeetMinimumNorcaVersion(k.srv.serf.Members(), k.srv.Region(), false) {
		return fmt.Errorf("all servers must be running Norca %v or later to approve webhooks", minNorcaFeaturesVersion)
	}

	ref := structs.ObjectKindRef(args.Group, args.Kind)
	existing, err := k.srv.State().ObjectKindByName(nil, args.Group, args.Kind)
	if err != nil {
		return err
	}
	if existing == nil {
		return structs.NewErrRPCCodedf(http.StatusNotFound, "object kind %q not found", ref)
	}
	kind := existing.Copy()
	w := kind.Webhook(args.Webhook)
	if w == nil {
		return structs.NewErrRPCCodedf(http.StatusNotFound, "object kind %q has no webhook %q", ref, args.Webhook)
	}
	if args.Revoke {
		if w.Approval == nil {
			reply.ObjectKind = existing
			reply.Index = existing.ModifyIndex
			return nil
		}
		w.Approval = nil
	} else {
		if w.Approved() {
			reply.ObjectKind = existing
			reply.Index = existing.ModifyIndex
			return nil
		}
		w.Approval = newWebhookApproval(w, admissionUser(args))
	}

	cas := existing.ModifyIndex
	req := &structs.ObjectKindUpsertRequest{ObjectKind: kind, CAS: &cas, WriteRequest: args.WriteRequest}
	_, index, err := k.srv.raftApply(structs.ObjectKindUpsertRequestType|structs.IgnoreUnknownTypeFlag, req)
	if err != nil {
		return err
	}
	k.srv.objects.forget(ref)
	kind.ModifyIndex = index
	reply.ObjectKind = kind
	reply.Index = index
	return nil
}

// objectMigrationLoop runs on the leader: it migrates the stored objects of
// kinds whose schema version was raised, a batch at a time.
func (s *Server) objectMigrationLoop(stopCh chan struct{}) {
	timer := time.NewTimer(objectMigrationInterval)
	defer timer.Stop()
	// failed remembers objects whose migration failed, by object ID, at the
	// modify indexes of the kind and the object: they are retried only when
	// one of them changes.
	failed := map[string]string{}
	for {
		select {
		case <-stopCh:
			return
		case <-timer.C:
		}
		if ServersMeetMinimumNorcaVersion(s.Members(), s.Region(), false) {
			if err := s.migrateObjectsOnce(stopCh, failed); err != nil {
				s.logger.Warn("object migration failed", "error", err)
			}
		}
		timer.Reset(objectMigrationInterval)
	}
}

// migrateObjectsOnce migrates up to objectMigrationBatch objects.
func (s *Server) migrateObjectsOnce(stopCh chan struct{}, failed map[string]string) error {
	snap, err := s.State().Snapshot()
	if err != nil {
		return err
	}
	kinds, err := snap.ObjectKinds(nil, "", state.SortDefault)
	if err != nil {
		return err
	}
	budget := objectMigrationBatch
	for raw := kinds.Next(); raw != nil && budget > 0; raw = kinds.Next() {
		kind := raw.(*structs.ObjectKind)
		if kind.Migration == nil {
			continue
		}
		iter, err := snap.CustomObjects(nil, kind.Group, kind.Kind, structs.AllNamespacesSentinel, state.SortDefault)
		if err != nil {
			return err
		}
		for rawObj := iter.Next(); rawObj != nil && budget > 0; rawObj = iter.Next() {
			obj := rawObj.(*structs.CustomObject)
			if obj.SchemaVersion() >= kind.SchemaVersion() {
				continue
			}
			key := fmt.Sprintf("%d/%d", kind.ModifyIndex, obj.ModifyIndex)
			if failed[obj.ID] == key {
				continue
			}
			select {
			case <-stopCh:
				return nil
			default:
			}
			budget--
			if err := s.migrateStoredObject(kind, obj); err != nil {
				if structs.IsObjectConflictError(err) {
					continue // changed meanwhile; the next round sees it
				}
				failed[obj.ID] = key
				s.logger.Warn("failed to migrate object", "kind", kind.KindRef(), "namespace", obj.Namespace,
					"name", obj.Name, "version", kind.SchemaVersion(), "error", err)
				_, _ = s.recordObjectEvents(obj, []*structs.ObjectEvent{{
					Type:   structs.ObjectEventTypeWarning,
					Reason: structs.ObjectMigrationFailedReason,
					Message: fmt.Sprintf("migration to schema version %d failed: %v", kind.SchemaVersion(),
						truncateString(err.Error(), 512)),
					Meta: map[string]string{"from_version": fmt.Sprint(obj.SchemaVersion()), "to_version": fmt.Sprint(kind.SchemaVersion())},
				}}, "server:migration")
				continue
			}
			metrics.IncrCounterWithLabels([]string{"nomad", "custom_object", "migrated"}, 1,
				[]metrics.Label{{Name: "kind", Value: kind.KindRef()}})
			time.Sleep(objectMigrationPause)
		}
	}
	// Forget failures of objects that no longer exist.
	if len(failed) > 10000 {
		clear(failed)
	}
	return nil
}

// migrateStoredObject migrates one object with check-and-set on its modify
// index.
func (s *Server) migrateStoredObject(kind *structs.ObjectKind, obj *structs.CustomObject) error {
	prepared, err := s.objects.eco.migrations.ForKind(kind)
	if err != nil {
		return err
	}
	schema, err := s.objects.schema(kind)
	if err != nil {
		return err
	}
	migrated, _, err := migrateObject(prepared, schema, kind, obj)
	if err != nil {
		return err
	}
	migrated.ModifyTime = time.Now().UnixNano()
	if err := validateObjectStatus(migrated); err != nil {
		return err
	}
	req := &structs.CustomObjectUpsertStateRequest{
		Object:              migrated,
		ExpectedModifyIndex: obj.ModifyIndex,
		WriteRequest:        structs.WriteRequest{Region: s.Region()},
	}
	_, _, err = s.raftApply(structs.CustomObjectUpsertRequestType|structs.IgnoreUnknownTypeFlag, req)
	return err
}

func truncateString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
