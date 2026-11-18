// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/plugins/csi"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-memdb"
)

// The operator of volume requests keeps a CSI volume as its CSIVolumeRequest
// declares it. A missing volume is created through the plugin (mode create)
// or registered (mode register); a raised capacity is expanded; what was
// changed by hand through the volume API (capabilities, mount options,
// capacity, secrets, the context of a registered volume) is set back.
//
// CSI volumes have no owner field. The request remembers the volume it
// manages by the create index of the volume in its status, and a volume of
// the same ID that it did not make is left alone unless the request says
// adopt. Right before it calls the plugin the request writes "creating" into
// its status, and that stays until the volume is made: a volume that appears
// while it is set, and is the volume the request makes (its plugin and name,
// for a registered one its external ID), is the request's own even if the
// write of its create index was lost.
//
// A created volume that disappears is not created again: an empty volume in
// its place would look like the old one to its jobs. The request reports it
// (VolumeLost) until it is deleted or written again.

const (
	// volumeRequestRetryBase and volumeRequestRetryMax bound the wait after
	// a failed create, register or expansion: it doubles with every failure
	// in a row. A test sets its own base in the configuration of its server.
	volumeRequestRetryBase = 30 * time.Second
	volumeRequestRetryMax  = 30 * time.Minute

	// volumeRequestInUsePoll is how often a deletion that waits for the
	// allocations of a volume looks again.
	volumeRequestInUsePoll = 30 * time.Second
)

// volumeRequestReasonUpdating is the reason of Ready=False of a pass whose
// update is made again in the next one. Like Creating it is not Degraded.
const volumeRequestReasonUpdating = "Updating"

// The conditions of a volume request besides Ready and Degraded.
const (
	volumeRequestConditionConflict = "Conflict"
	volumeRequestConditionFailed   = "Failed"
)

// volumeRequestStatus is the status of a CSIVolumeRequest object.
type volumeRequestStatus struct {
	VolumeCreateIndex  uint64 `json:"volumeCreateIndex,omitempty"`
	Creating           bool   `json:"creating,omitempty"`
	ExternalID         string `json:"externalID,omitempty"`
	Provider           string `json:"provider,omitempty"`
	CapacityBytes      int64  `json:"capacityBytes"`
	Schedulable        bool   `json:"schedulable"`
	ReadAllocs         int    `json:"readAllocs"`
	WriteAllocs        int    `json:"writeAllocs"`
	ControllersHealthy int    `json:"controllersHealthy"`
	NodesHealthy       int    `json:"nodesHealthy"`
	LastDrift          string `json:"lastDrift,omitempty"`

	// AppliedGeneration is the generation of the request the volume was
	// last seen or made as it declares. While it is behind the generation
	// of the object, a difference is an update that is still to be done,
	// not drift.
	AppliedGeneration uint64 `json:"appliedGeneration,omitempty"`

	Drifts   int    `json:"drifts"`
	Failures int    `json:"failures"`
	RetryAt  string `json:"retryAt,omitempty"`

	// LastFailure is the error of the last failed attempt while its retry
	// is waited for. The Failed condition is not the place to read it back
	// from: a pass that ends before the attempt (no plugin, no secret)
	// writes that condition false.
	LastFailure string `json:"lastFailure,omitempty"`
}

// noFailure forgets the failures of the attempts so far.
func (st *volumeRequestStatus) noFailure() {
	st.Failures, st.RetryAt, st.LastFailure = 0, "", ""
}

// volumeCSI is how the operator reaches the controller plugins. Tests
// replace it.
type volumeCSI interface {
	// create creates the volume in the storage and fills in what the
	// plugin answers (external ID, capacity, context, topologies).
	create(vol *structs.CSIVolume, plugin *structs.CSIPlugin) error

	// validate asks the plugin whether a registered volume exists with the
	// capabilities asked for.
	validate(vol *structs.CSIVolume, plugin *structs.CSIPlugin) error

	// expand brings the capacity of the volume to the range; vol is changed
	// to what the plugin answers.
	expand(vol *structs.CSIVolume, plugin *structs.CSIPlugin, capacity *csi.CapacityRange) error

	// delete deletes the volume in the storage.
	delete(vol *structs.CSIVolume, plugin *structs.CSIPlugin) error
}

// endpointVolumeCSI reaches the plugins as the volume RPCs do.
type endpointVolumeCSI struct{ v *CSIVolume }

func (e endpointVolumeCSI) create(vol *structs.CSIVolume, plugin *structs.CSIPlugin) error {
	return e.v.createVolume(vol, plugin)
}

func (e endpointVolumeCSI) validate(vol *structs.CSIVolume, plugin *structs.CSIPlugin) error {
	return e.v.controllerValidateVolume(nil, vol, plugin)
}

func (e endpointVolumeCSI) expand(vol *structs.CSIVolume, plugin *structs.CSIPlugin, capacity *csi.CapacityRange) error {
	return e.v.expandVolume(vol, plugin, capacity)
}

func (e endpointVolumeCSI) delete(vol *structs.CSIVolume, plugin *structs.CSIPlugin) error {
	return e.v.deleteVolume(vol.Copy(), plugin, nil)
}

// volumeRequestOperator is the built-in operator of volume requests.
type volumeRequestOperator struct {
	srv    *Server
	logger hclog.Logger
	csi    volumeCSI

	// retryBase is the wait after the first failed attempt.
	retryBase time.Duration

	// turns ends a pass that is out of its budget, or of a server that is
	// no longer the leader, between two requests.
	turns operatorTurns
}

func newVolumeRequestOperator(srv *Server, logger hclog.Logger) *volumeRequestOperator {
	var csi volumeCSI = endpointVolumeCSI{v: NewCSIVolumeEndpoint(srv, nil)}
	if srv.config != nil && srv.config.volumeCSI != nil {
		csi = srv.config.volumeCSI
	}
	retryBase := volumeRequestRetryBase
	if srv.config != nil && srv.config.volumeRequestRetryBase > 0 {
		retryBase = srv.config.volumeRequestRetryBase
	}
	return &volumeRequestOperator{srv: srv, logger: logger.Named("volumes"), csi: csi, retryBase: retryBase,
		turns: operatorTurns{srv: srv, budget: operatorPassBudget}}
}

func (o *volumeRequestOperator) kinds() []*structs.ObjectKind { return builtinVolumeRequestKinds() }

// volumeRequestPass is what one pass of the operator read.
type volumeRequestPass struct {
	now   time.Time
	store *state.StateStore
	ws    memdb.WatchSet
	wish  time.Time
}

func (p *volumeRequestPass) want(t time.Time) {
	if !t.IsZero() && (p.wish.IsZero() || t.Before(p.wish)) {
		p.wish = t
	}
}

func (o *volumeRequestOperator) reconcile(ws memdb.WatchSet) (time.Time, error) {
	store := o.srv.State()
	p := &volumeRequestPass{now: time.Now().UTC(), store: store, ws: ws}

	objs, err := listObjects(store, ws, csiVolumeRequestKind, structs.AllNamespacesSentinel)
	if err != nil {
		return time.Time{}, err
	}
	// A request may take a call of its CSI plugin, which may take minutes:
	// a server that is no longer the leader makes no call for the requests
	// it has not come to, and a pass out of its budget leaves them to the
	// next one, which begins with them.
	started := time.Now()
	for i, obj := range o.turns.order(objs) {
		goOn, again := o.turns.next(started, i, obj)
		if again {
			p.want(time.Now().Add(operatorsSettle))
		}
		if !goOn {
			break
		}
		o.reconcileRequest(p, obj)
	}
	return p.wish, nil
}

// volumeRequestOutcome is what a pass found out about a request, for its
// conditions.
type volumeRequestOutcome struct {
	// notReadyReason and notReadyMessage say why the volume is not as the
	// request declares it.
	notReadyReason, notReadyMessage string

	// conflict is the message of a volume the request does not own.
	conflict string

	// failed is the error of the last attempt, if it failed.
	failed string
}

func (out *volumeRequestOutcome) notReady(reason, format string, args ...any) {
	if out.notReadyReason == "" {
		out.notReadyReason, out.notReadyMessage = reason, fmt.Sprintf(format, args...)
	}
}

// reconcileRequest brings one request to its wanted state.
func (o *volumeRequestOperator) reconcileRequest(p *volumeRequestPass, obj *structs.CustomObject) {
	// The request was read when the pass began, and the requests before
	// it may have taken long: one that was deleted or written since is
	// not acted on as it was (see operatorObjectCurrent).
	if !o.srv.operatorObjectCurrent(obj) {
		return
	}
	spec := &volumeRequestSpec{}
	if err := structs.DecodeObjectPart(obj.Spec, spec); err != nil {
		o.logger.Warn("volume request with a spec that can't be decoded", "namespace", obj.Namespace,
			"request", obj.Name, "error", err)
		return
	}
	st := &volumeRequestStatus{}
	if err := structs.DecodeObjectPart(obj.Status, st); err != nil {
		if obj.Deleting() && spec.policy() == volumeRequestRetain {
			// The volume stays: what the status says of it is not
			// needed to let the request go.
			o.finishDeletion(p, obj, spec, &volumeRequestStatus{}, nil, nil, false)
			return
		}
		o.holdUndecodableStatus(p, obj, err)
		return
	}

	vol, err := p.store.CSIVolumeByID(p.ws, obj.Namespace, obj.Name)
	if err != nil {
		o.logger.Warn("volume lookup failed", "namespace", obj.Namespace, "volume", obj.Name, "error", err)
		p.want(p.now.Add(operatorsRetry))
		return
	}
	plugin, err := p.store.CSIPluginByID(p.ws, spec.PluginID)
	if err != nil {
		o.logger.Warn("plugin lookup failed", "plugin", spec.PluginID, "error", err)
		p.want(p.now.Add(operatorsRetry))
		return
	}

	// The volume is the request's own when it has the create index the
	// request remembers, or appeared while the request was creating it and
	// is what the request creates. Any other volume of the ID was made by
	// somebody else in the meantime.
	ours := vol != nil && st.VolumeCreateIndex != 0 && vol.CreateIndex == st.VolumeCreateIndex
	if vol != nil && !ours && st.Creating && spec.makes(obj, vol) {
		ours = true
		st.VolumeCreateIndex, st.Creating = vol.CreateIndex, false
	}

	if obj.Deleting() {
		o.finishDeletion(p, obj, spec, st, vol, plugin, ours)
		return
	}

	// The wait after a failure is the wait of the spec that failed: a
	// changed spec is tried at once.
	if obj.Status != nil && statusGeneration(obj.Status) != int64(obj.Generation) {
		st.noFailure()
	}

	out := &volumeRequestOutcome{}
	finalizers := slices.Clone(obj.Finalizers)
	if !slices.Contains(finalizers, structs.OperatorsFinalizer) {
		finalizers = append(finalizers, structs.OperatorsFinalizer)
	}

	secrets, secretErr := o.secrets(obj, spec)
	switch {
	case spec.validate() != nil:
		out.notReady("InvalidSpec", "%v", spec.validate())
	case secretErr != nil:
		out.notReady("SecretUnavailable", "%v", secretErr)

	case vol != nil && !ours && !spec.Adopt:
		out.conflict = fmt.Sprintf("the volume %q exists and was not made by this request; set adopt = true to take it over", obj.Name)
		out.notReady(volumeRequestConditionConflict, "%s", out.conflict)

	case vol != nil && !ours && vol.PluginID != spec.PluginID:
		out.conflict = fmt.Sprintf("the volume %q is of the plugin %q, not %q", obj.Name, vol.PluginID, spec.PluginID)
		out.notReady(volumeRequestConditionConflict, "%s", out.conflict)

	case vol != nil && !ours:
		// Adopted: the volume is the request's from now on.
		st.VolumeCreateIndex, st.Creating = vol.CreateIndex, false
		o.srv.operatorEvent(obj, structs.ObjectEventTypeNormal, "Adopted",
			fmt.Sprintf("the volume %s (create index %d) is kept by this request now", obj.Name, vol.CreateIndex))
		o.keep(p, obj, spec, st, vol, plugin, secrets, true, out)

	case vol != nil:
		o.keep(p, obj, spec, st, vol, plugin, secrets, false, out)

	case st.VolumeCreateIndex != 0 && spec.mode() == volumeRequestModeCreate:
		out.notReady("VolumeLost", "the volume %q this request created is gone (deregistered or deleted by hand); "+
			"it is not created again, so its jobs don't get an empty volume in its place: delete the request and write it again", obj.Name)

	default:
		obj = o.make(p, obj, spec, st, plugin, secrets, out, &finalizers)
	}

	if vol, err = p.store.CSIVolumeByID(nil, obj.Namespace, obj.Name); err == nil && vol != nil &&
		st.VolumeCreateIndex == vol.CreateIndex {
		volumeStatusFrom(st, vol)
	}
	o.writeRequest(p, obj, st, out, &finalizers)
}

// holdUndecodableStatus leaves a request whose status can't be decoded (a
// server of another version wrote it) as it is: the status says which
// volume is the request's own, and an empty one in its place would take
// that volume for somebody else's. Nothing is created, changed or deleted,
// and a deleted request that would deregister or delete its volume is not
// let go, until a status that can be decoded is written; the conditions say
// so (best effort: a status the kind's schema refuses can't take them).
func (o *volumeRequestOperator) holdUndecodableStatus(p *volumeRequestPass, obj *structs.CustomObject, err error) {
	o.logger.Warn("volume request with a status that can't be decoded is left alone", "namespace", obj.Namespace,
		"request", obj.Name, "error", err)
	status := structs.CopyObjectMap(obj.Status)
	conditions := newConditionSet(obj.Status, p.now)
	conditions.set(structs.CertificateConditionReady, false, "InvalidStatus",
		fmt.Sprintf("the status can't be decoded (%v): it says which volume is the request's own, so it is kept, "+
			"and the volume is not created, changed or deleted until it can be", err))
	conditions.set(structs.CertificateConditionDegraded, true, "", "")
	status[structs.ObjectStatusConditions] = conditions.list()
	normalized, err := structs.NormalizeObjectMap(status)
	if err != nil || structs.ObjectMapsEqual(obj.Status, normalized) {
		return
	}
	changed := obj.Copy()
	changed.Status = normalized
	if _, err := o.srv.writeOperatorObject(changed); err != nil && !structs.IsObjectConflictError(err) {
		o.logger.Warn("failed to write the status of a volume request", "request", obj.Name,
			"namespace", obj.Namespace, "error", err)
	}
}

// deleteUnregistered deletes the volume that a create of the request may have
// left in the storage without a volume registered for it. The plugin is
// asked for the volume once more: a create of the same name answers with the
// volume that exists (the name is the idempotency key of CSI CreateVolume),
// which gives its external ID, and that is deleted. It reports whether the
// deletion of the request can go on; when the volume can't be asked for, the
// request goes and a warning says what may be left.
func (o *volumeRequestOperator) deleteUnregistered(obj *structs.CustomObject, spec *volumeRequestSpec,
	plugin *structs.CSIPlugin, hold func(reason, format string, args ...any)) bool {

	leave := func(format string, args ...any) bool {
		name := spec.volume(obj, nil).Name
		message := fmt.Sprintf("a create of the volume was sent to the CSI plugin %q and no volume was registered: "+
			"a volume named %q may be left in the storage (%s)", spec.PluginID, name, fmt.Sprintf(format, args...))
		o.logger.Warn("deleted volume request may leave a volume in the storage", "namespace", obj.Namespace,
			"request", obj.Name, "plugin", spec.PluginID, "volume_name", name, "reason", fmt.Sprintf(format, args...))
		o.srv.operatorEvent(obj, structs.ObjectEventTypeWarning, "VolumeMayBeLeft", message)
		return true
	}

	if err := spec.validate(); err != nil {
		return leave("the request is not valid: %v", err)
	}
	// As for a registered volume: the deletion waits for the plugin.
	out := &volumeRequestOutcome{}
	if !pluginReady(spec, plugin, out) {
		if out.notReadyReason == "Unsupported" {
			return leave("%s", out.notReadyMessage)
		}
		hold(out.notReadyReason, "a volume this request was creating may be in the storage, and can't be deleted now: %s", out.notReadyMessage)
		return false
	}
	secrets, err := o.secrets(obj, spec)
	if err != nil {
		return leave("%v", err)
	}
	vol := spec.volume(obj, secrets)
	if err := pluginSupportsAccessModes(plugin, vol); err != nil {
		return leave("%v", err)
	}
	vol.Provider, vol.ProviderVersion = plugin.Provider, plugin.Version
	if err := o.csi.create(vol, plugin); err != nil {
		return leave("asking the plugin for it again failed: %v", err)
	}
	// The call may have taken long: a server that is no longer the leader
	// deletes nothing. The request stays, for the leader.
	if !o.srv.operatorObjectCurrent(obj) {
		return false
	}
	other, err := o.registeredElsewhere(vol)
	if err != nil {
		return leave("looking up the volume %q failed: %v", vol.ExternalID, err)
	}
	if other != nil {
		// The name is the name of a volume of somebody else: nothing of
		// this request is in the storage.
		return true
	}
	if err := o.csi.delete(vol, plugin); err != nil {
		o.srv.operatorEvent(obj, structs.ObjectEventTypeWarning, "DeleteFailed", err.Error())
		hold("DeleteFailed", "deleting the volume %q failed: %v", vol.ExternalID, err)
		return false
	}
	operatorCount(csiVolumeRequestKind, "volumes_deleted")
	return true
}

// volumeStatusFrom copies what the status shows of the volume.
func volumeStatusFrom(st *volumeRequestStatus, vol *structs.CSIVolume) {
	st.ExternalID = vol.ExternalID
	st.Provider = vol.Provider
	st.CapacityBytes = vol.Capacity
	st.Schedulable = vol.Schedulable
	st.ReadAllocs = len(vol.ReadAllocs)
	st.WriteAllocs = len(vol.WriteAllocs)
	st.ControllersHealthy = vol.ControllersHealthy
	st.NodesHealthy = vol.NodesHealthy
}

// retrying reports whether the last attempt failed and its retry is not
// due yet. The request is failed until then.
func (o *volumeRequestOperator) retrying(p *volumeRequestPass, st *volumeRequestStatus, out *volumeRequestOutcome) bool {
	retryAt, err := time.Parse(time.RFC3339, st.RetryAt)
	if err != nil || !p.now.Before(retryAt) {
		return false
	}
	out.failed = st.LastFailure
	if out.failed == "" {
		out.failed = "the last attempt failed; the next one is made at " + st.RetryAt
	}
	p.want(retryAt)
	return true
}

// failure records a failed attempt.
func (o *volumeRequestOperator) failure(p *volumeRequestPass, obj *structs.CustomObject, st *volumeRequestStatus,
	out *volumeRequestOutcome, what string, err error) {

	st.Failures++
	wait := min(o.retryBase<<min(st.Failures-1, 16), volumeRequestRetryMax)
	retryAt := p.now.Add(wait)
	st.RetryAt = retryAt.Format(time.RFC3339)
	out.failed = fmt.Sprintf("%s: %v", what, err)
	st.LastFailure = out.failed
	if len(st.LastFailure) > 1000 {
		st.LastFailure = st.LastFailure[:1000]
	}
	p.want(retryAt)
	operatorCount(csiVolumeRequestKind, "failures")
	o.srv.operatorEvent(obj, structs.ObjectEventTypeWarning, "Failed",
		fmt.Sprintf("%s (attempt %d): %v", what, st.Failures, err))
}

// pluginReady checks that the plugin can do what the mode needs.
func pluginReady(spec *volumeRequestSpec, plugin *structs.CSIPlugin, out *volumeRequestOutcome) bool {
	switch {
	case plugin == nil:
		out.notReady("PluginNotFound", "no CSI plugin %q", spec.PluginID)
	case plugin.ControllerRequired && plugin.ControllersHealthy < 1:
		out.notReady("PluginUnavailable", "no healthy controller of the CSI plugin %q", spec.PluginID)
	case spec.mode() == volumeRequestModeCreate && !plugin.ControllerRequired:
		out.notReady("Unsupported", "the CSI plugin %q has no controller to create volumes", spec.PluginID)
	case spec.mode() == volumeRequestModeCreate && !plugin.HasControllerCapability(structs.CSIControllerSupportsCreateDelete):
		out.notReady("Unsupported", "the controller of the CSI plugin %q can't create volumes", spec.PluginID)
	default:
		return true
	}
	return false
}

// make creates or registers the volume of a request. It returns the object
// as it is stored now: the intent is written before the plugin is called.
func (o *volumeRequestOperator) make(p *volumeRequestPass, obj *structs.CustomObject, spec *volumeRequestSpec,
	st *volumeRequestStatus, plugin *structs.CSIPlugin, secrets structs.CSISecrets, out *volumeRequestOutcome,
	finalizers *[]string) *structs.CustomObject {

	if !pluginReady(spec, plugin, out) {
		p.want(p.now.Add(operatorsRetry))
		return obj
	}
	if o.retrying(p, st, out) {
		return obj
	}
	vol := spec.volume(obj, secrets)
	if err := pluginSupportsAccessModes(plugin, vol); err != nil {
		out.notReady("Unsupported", "%v", err)
		return obj
	}
	vol.Provider, vol.ProviderVersion = plugin.Provider, plugin.Version

	// The intent first, so the volume is known as the request's own even
	// if the write after the create is lost. It is written only here, when
	// the plugin is called next: a request that waits for its plugin or
	// its secret has sent nothing, and a volume of its ID that somebody
	// makes in that time is not its own. Once set it stays until the
	// volume is made: an attempt that failed may have made the volume.
	if !st.Creating {
		st.Creating = true
		intent := &volumeRequestOutcome{}
		intent.notReady("Creating", "creating the volume")
		written := o.writeRequest(p, obj, st, intent, finalizers)
		if written == nil {
			st.Creating = false
			out.notReady("Creating", "creating the volume")
			p.want(p.now.Add(operatorsRetry))
			return obj
		}
		obj = written
	}

	// No write came before the call when the intent was there already (a
	// retry): the request is looked at once more, so no volume is made for
	// a request that was deleted or changed since the pass read it.
	if !o.srv.operatorObjectCurrent(obj) {
		out.notReady("Creating", "creating the volume")
		return obj
	}

	what := "create the volume"
	if spec.mode() == volumeRequestModeRegister {
		what = "register the volume"
		if err := o.csi.validate(vol, plugin); err != nil {
			o.failure(p, obj, st, out, what, err)
			return obj
		}
	} else if err := o.csi.create(vol, plugin); err != nil {
		o.failure(p, obj, st, out, what, err)
		return obj
	} else if other, err := o.registeredElsewhere(vol); err != nil {
		o.failure(p, obj, st, out, what, err)
		return obj
	} else if other != nil {
		// The plugin answered a create of a name it knows with the volume
		// it has under that name. It is somebody else's: not registered a
		// second time, and never deleted with this request.
		o.failure(p, obj, st, out, what, fmt.Errorf("the plugin answered the name %q with the volume %q of the storage, "+
			"which is registered as %s/%s: the request needs a display_name of its own",
			vol.Name, vol.ExternalID, other.Namespace, other.ID))
		return obj
	}

	// A register writes the volume as a whole. A volume of the ID that
	// appeared while the plugin worked is not written over: the next pass
	// sees whose it is.
	if appeared, err := o.srv.State().CSIVolumeByID(nil, vol.Namespace, vol.ID); err != nil {
		o.failure(p, obj, st, out, what, err)
		return obj
	} else if appeared != nil {
		out.notReady("Creating", "a volume %q appeared while it was made", obj.Name)
		p.want(p.now.Add(operatorsSettle))
		return obj
	}

	index, err := o.register(vol, 0)
	if structs.IsCSIVolumeConflictError(err) {
		// The same, after the look above: not a failure.
		out.notReady("Creating", "a volume %q appeared while it was made", obj.Name)
		p.want(p.now.Add(operatorsSettle))
		return obj
	}
	if err != nil {
		o.failure(p, obj, st, out, what, err)
		return obj
	}
	st.VolumeCreateIndex, st.Creating = index, false
	st.AppliedGeneration = obj.Generation
	st.noFailure()
	reason := "Created"
	if spec.mode() == volumeRequestModeRegister {
		reason = "Registered"
	}
	operatorCount(csiVolumeRequestKind, "volumes_"+strings.ToLower(reason))
	o.srv.operatorEvent(obj, structs.ObjectEventTypeNormal, reason,
		fmt.Sprintf("the volume %s (external ID %s) with the plugin %s", vol.ID, vol.ExternalID, vol.PluginID))
	return obj
}

// registeredElsewhere returns a registered volume of any namespace that is
// the same volume of the storage (plugin and external ID) as vol under
// another ID, if there is one.
func (o *volumeRequestOperator) registeredElsewhere(vol *structs.CSIVolume) (*structs.CSIVolume, error) {
	iter, err := o.srv.State().CSIVolumes(nil)
	if err != nil {
		return nil, err
	}
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		other := raw.(*structs.CSIVolume)
		if other.PluginID == vol.PluginID && other.ExternalID == vol.ExternalID &&
			(other.Namespace != vol.Namespace || other.ID != vol.ID) {
			return other, nil
		}
	}
	return nil, nil
}

// register writes a volume to the state, as a volume register does, and
// returns its create index. A register writes the volume as a whole, with
// its claims, so it is a check-and-set: expected is the modify index of the
// stored volume that vol was built from, 0 if there was none. If the volume
// was written in between (an allocation claimed or released it), nothing is
// written and the error is a conflict (structs.IsCSIVolumeConflictError):
// the next pass does the work again from the volume as it is then.
func (o *volumeRequestOperator) register(vol *structs.CSIVolume, expected uint64) (uint64, error) {
	if o.srv.config != nil && o.srv.config.volumeRequestBeforeRegister != nil {
		o.srv.config.volumeRequestBeforeRegister(vol)
	}
	req := &structs.CSIVolumeRegisterRequest{
		Volumes:             []*structs.CSIVolume{vol},
		ExpectedModifyIndex: map[string]uint64{vol.ID: expected},
		WriteRequest:        structs.WriteRequest{Region: o.srv.Region(), Namespace: vol.Namespace},
	}
	if _, _, err := o.srv.raftApply(structs.CSIVolumeRegisterRequestType, req); err != nil {
		return 0, err
	}
	written, err := o.srv.State().CSIVolumeByID(nil, vol.Namespace, vol.ID)
	if err != nil {
		return 0, err
	}
	if written == nil {
		return 0, errors.New("the volume is not in the state after its registration")
	}
	return written.CreateIndex, nil
}

// keep sets a volume of the request back to what the spec declares.
func (o *volumeRequestOperator) keep(p *volumeRequestPass, obj *structs.CustomObject, spec *volumeRequestSpec,
	st *volumeRequestStatus, vol *structs.CSIVolume, plugin *structs.CSIPlugin, secrets structs.CSISecrets,
	adopted bool, out *volumeRequestOutcome) {

	want := spec.volume(obj, secrets)
	diff := volumeDiff(vol, want, spec.mode() == volumeRequestModeRegister)
	if len(diff) == 0 {
		st.AppliedGeneration = obj.Generation
		st.noFailure()
		if !vol.Schedulable {
			out.notReady("NotSchedulable", "the volume is registered but not schedulable (its plugin is not healthy)")
		}
		return
	}
	if plugin == nil {
		out.notReady("PluginNotFound", "no CSI plugin %q", spec.PluginID)
		p.want(p.now.Add(operatorsRetry))
		return
	}
	if o.retrying(p, st, out) {
		return
	}

	// A change of the spec is an update, until it is done: its first
	// attempt may fail. So is a difference of the secrets alone: they
	// follow the secret store, which changes without the request. Anything
	// else was changed through the volume API, and is set back.
	updated := adopted || st.AppliedGeneration != obj.Generation || (len(diff) == 1 && diff[0] == "secrets")

	// follow returns a copy of a stored volume with what the spec declares.
	// Merge refuses changes of what a volume can't change, and takes the
	// rest. The name of an adopted volume stays its own, and the context of
	// a created volume is the plugin's.
	follow := func(stored *structs.CSIVolume) (*structs.CSIVolume, error) {
		changed := stored.Copy()
		mergeIn := *want
		mergeIn.Name = ""
		if spec.mode() == volumeRequestModeCreate {
			mergeIn.Context = maps.Clone(stored.Context)
		}
		// Merge refuses a change of the mount options of a volume in use,
		// and takes none (nil) and empty ones for a change: the stored
		// volume has empty ones where the request has none.
		if mountOptionsEqual(changed.MountOptions, mergeIn.MountOptions) {
			mergeIn.MountOptions = changed.MountOptions
		}
		if err := changed.Merge(&mergeIn); err != nil {
			return nil, err
		}
		changed.Secrets = maps.Clone(want.Secrets)
		if spec.mode() == volumeRequestModeRegister {
			changed.Context = maps.Clone(want.Context)
		}
		return changed, nil
	}

	expanded, err := follow(vol)
	if err != nil {
		o.failure(p, obj, st, out, "update the volume", err)
		return
	}
	if !o.srv.operatorObjectCurrent(obj) {
		out.notReady(volumeRequestReasonUpdating, "the request changed while it was looked at")
		return
	}
	if err := o.csi.expand(expanded, plugin, &csi.CapacityRange{
		RequiredBytes: want.RequestedCapacityMin,
		LimitBytes:    want.RequestedCapacityMax,
	}); err != nil {
		o.failure(p, obj, st, out, "expand the volume", err)
		return
	}

	// The plugin call can take minutes, and the volume is written as a
	// whole, with its claims. What is written is therefore the volume as it
	// is now, not the copy this pass began with: a claim made or released
	// while the plugin worked stays as it is. A volume that went away in
	// that time is not written back; the next pass sees that it is gone.
	live, err := o.srv.State().CSIVolumeByID(nil, vol.Namespace, vol.ID)
	if err != nil {
		o.failure(p, obj, st, out, "update the volume", err)
		return
	}
	if live == nil || live.CreateIndex != vol.CreateIndex {
		out.notReady("VolumeLost", "the volume %q went away while it was updated", obj.Name)
		p.want(p.now.Add(operatorsSettle))
		return
	}
	changed, err := follow(live)
	if err != nil {
		o.failure(p, obj, st, out, "update the volume", err)
		return
	}
	// The plugin may refuse nothing and change nothing (no expansion
	// needed): the requested values are the spec's either way.
	changed.Capacity = expanded.Capacity
	changed.RequestedCapacityMin, changed.RequestedCapacityMax = want.RequestedCapacityMin, want.RequestedCapacityMax

	_, err = o.register(changed, live.ModifyIndex)
	if structs.IsCSIVolumeConflictError(err) {
		// The volume was written after it was read here: a claim was made
		// or released. Nothing was written and nothing failed; the next
		// pass makes the update from the volume as it is then.
		out.notReady(volumeRequestReasonUpdating, "the volume %q changed while it was updated: the update is made again", obj.Name)
		p.want(p.now.Add(operatorsSettle))
		return
	}
	if err != nil {
		o.failure(p, obj, st, out, "update the volume", err)
		return
	}
	st.AppliedGeneration = obj.Generation
	st.noFailure()
	if updated {
		o.srv.operatorEvent(obj, structs.ObjectEventTypeNormal, "Updated",
			fmt.Sprintf("the volume follows the request: %s", strings.Join(diff, ", ")))
		return
	}
	st.Drifts++
	st.LastDrift = p.now.Format(time.RFC3339)
	operatorCount(csiVolumeRequestKind, "drifts")
	o.srv.operatorEvent(obj, structs.ObjectEventTypeWarning, "DriftCorrected",
		fmt.Sprintf("the volume was changed outside its request and was set back: %s", strings.Join(diff, ", ")))
}

// statusGeneration returns the observed generation of a status.
func statusGeneration(status map[string]any) int64 {
	switch v := status[structs.ObjectStatusObservedGeneration].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case uint64:
		return int64(v)
	}
	return -1
}

// secrets reads the CSI secrets of a request from the secret store.
func (o *volumeRequestOperator) secrets(obj *structs.CustomObject, spec *volumeRequestSpec) (structs.CSISecrets, error) {
	if spec.SecretPath == "" {
		return nil, nil
	}
	items, _, err := o.srv.secretReadValue(obj.Namespace, spec.SecretPath, 0)
	if err != nil {
		return nil, fmt.Errorf("the secret %q can't be read: %v", spec.SecretPath, err)
	}
	return structs.CSISecrets(items), nil
}

// volumeRequestFailure returns the message of the Failed condition of a
// status, if it is true.
func volumeRequestFailure(status map[string]any) string {
	for _, c := range operatorConditions(status) {
		if c.Type == volumeRequestConditionFailed && c.Status == "True" {
			return c.Message
		}
	}
	return ""
}

// writeRequest writes the status of a request with its conditions. It
// returns the object as it is stored then, nil if the write failed.
func (o *volumeRequestOperator) writeRequest(p *volumeRequestPass, obj *structs.CustomObject, st *volumeRequestStatus,
	out *volumeRequestOutcome, finalizers *[]string) *structs.CustomObject {

	encoded, err := structs.EncodeObjectPart(st)
	if err != nil {
		o.logger.Warn("status of a volume request can't be encoded", "request", obj.Name, "error", err)
		return nil
	}
	conditions := newConditionSet(obj.Status, p.now)
	ready := out.notReadyReason == "" && out.failed == ""
	reason, message := out.notReadyReason, out.notReadyMessage
	if reason == "" && out.failed != "" {
		reason, message = volumeRequestConditionFailed, out.failed
	}
	conditions.set(structs.CertificateConditionReady, ready, reason, message)
	conditions.set(volumeRequestConditionConflict, out.conflict != "", "", out.conflict)
	conditions.set(volumeRequestConditionFailed, out.failed != "", "", out.failed)
	conditions.set(structs.CertificateConditionDegraded,
		!ready && out.notReadyReason != "Creating" && out.notReadyReason != volumeRequestReasonUpdating, "", "")
	encoded[structs.ObjectStatusConditions] = conditions.list()
	encoded[structs.ObjectStatusObservedGeneration] = obj.Generation

	normalized, err := structs.NormalizeObjectMap(encoded)
	if err != nil {
		o.logger.Warn("status of a volume request can't be encoded", "request", obj.Name, "error", err)
		return nil
	}
	if structs.ObjectMapsEqual(obj.Status, normalized) && (finalizers == nil || slices.Equal(obj.Finalizers, *finalizers)) {
		return obj
	}
	changed := obj.Copy()
	changed.Status = normalized
	if finalizers != nil {
		changed.Finalizers = slices.Clone(*finalizers)
	}
	written, err := o.srv.writeOperatorObject(changed)
	if err != nil {
		if !structs.IsObjectConflictError(err) {
			o.logger.Warn("failed to write the status of a volume request", "request", obj.Name,
				"namespace", obj.Namespace, "error", err)
		}
		return nil
	}
	return written
}

// finishDeletion does what the deletion of a request asks of the operator:
// with "deregister" the volume leaves Norca, with "delete" it is deleted in
// the storage once no allocation uses it, with "retain" it stays. A volume
// the request does not own is never touched. With "delete", a volume that a
// create without a known answer may have left in the storage is deleted too.
func (o *volumeRequestOperator) finishDeletion(p *volumeRequestPass, obj *structs.CustomObject, spec *volumeRequestSpec,
	st *volumeRequestStatus, vol *structs.CSIVolume, plugin *structs.CSIPlugin, ours bool) {

	if !slices.Contains(obj.Finalizers, structs.OperatorsFinalizer) {
		return
	}
	hold := func(reason, format string, args ...any) {
		out := &volumeRequestOutcome{}
		out.notReady(reason, format, args...)
		p.want(p.now.Add(volumeRequestInUsePoll))
		o.writeRequest(p, obj, st, out, nil)
	}

	if ours && spec.policy() != volumeRequestRetain {
		if claims := len(vol.ReadClaims) + len(vol.WriteClaims) + len(vol.PastClaims); vol.InUse() || claims > 0 {
			hold("VolumeInUse", "the volume is claimed by %d allocation(s): it goes when they have stopped and released it",
				max(claims, len(vol.ReadAllocs)+len(vol.WriteAllocs)))
			return
		}
		if spec.policy() == volumeRequestDelete {
			if plugin == nil {
				hold("PluginNotFound", "the volume can't be deleted without its CSI plugin %q", vol.PluginID)
				return
			}
			if err := o.csi.delete(vol, plugin); err != nil {
				o.srv.operatorEvent(obj, structs.ObjectEventTypeWarning, "DeleteFailed", err.Error())
				hold("DeleteFailed", "deleting the volume failed: %v", err)
				return
			}
		}
		req := &structs.CSIVolumeDeregisterRequest{
			VolumeIDs:    []string{vol.ID},
			WriteRequest: structs.WriteRequest{Region: o.srv.Region(), Namespace: vol.Namespace},
		}
		if _, _, err := o.srv.raftApply(structs.CSIVolumeDeregisterRequestType, req); err != nil {
			hold("DeleteFailed", "deregistering the volume failed: %v", err)
			return
		}
		operatorCount(csiVolumeRequestKind, "volumes_"+map[string]string{
			volumeRequestDeregister: "deregistered", volumeRequestDelete: "deleted"}[spec.policy()])
	}

	// A create was sent to the plugin and no volume was registered: its
	// answer was lost or it failed, and the storage may have the volume.
	if vol == nil && st.Creating && st.VolumeCreateIndex == 0 &&
		spec.mode() == volumeRequestModeCreate && spec.policy() == volumeRequestDelete {
		if !o.deleteUnregistered(obj, spec, plugin, hold) {
			return
		}
	}

	changed := obj.Copy()
	changed.Finalizers = slices.DeleteFunc(changed.Finalizers, func(f string) bool { return f == structs.OperatorsFinalizer })
	written, err := o.srv.writeOperatorObject(changed)
	if err != nil {
		if !structs.IsObjectConflictError(err) {
			o.logger.Warn("failed to remove the finalizer of a deleted volume request", "request", obj.Name, "error", err)
		}
		return
	}
	o.srv.finalizeObjectIfReady(written)
}
