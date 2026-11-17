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
// adopt. Before a create the request writes "creating" into its status, so a
// volume that appears while that is set is the request's own even if the
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
	Drifts             int    `json:"drifts"`
	Failures           int    `json:"failures"`
	RetryAt            string `json:"retryAt,omitempty"`
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
	return &volumeRequestOperator{srv: srv, logger: logger.Named("volumes"), csi: csi, retryBase: retryBase}
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
	for _, obj := range objs {
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
	spec := &volumeRequestSpec{}
	if err := structs.DecodeObjectPart(obj.Spec, spec); err != nil {
		o.logger.Warn("volume request with a spec that can't be decoded", "namespace", obj.Namespace,
			"request", obj.Name, "error", err)
		return
	}
	st := &volumeRequestStatus{}
	if err := structs.DecodeObjectPart(obj.Status, st); err != nil {
		st = &volumeRequestStatus{}
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
	// request remembers, or appeared while the request was creating it.
	ours := vol != nil && st.VolumeCreateIndex != 0 && vol.CreateIndex == st.VolumeCreateIndex
	if vol != nil && !ours && st.Creating {
		ours = true
		st.VolumeCreateIndex, st.Creating = vol.CreateIndex, false
	}

	if obj.Deleting() {
		o.finishDeletion(p, obj, spec, st, vol, plugin, ours)
		return
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

	case !st.Creating:
		// The intent first, so the volume is known as the request's own
		// even if the write after the create is lost.
		st.Creating = true
		out.notReady("Creating", "creating the volume")
		p.want(p.now)

	default:
		o.make(p, obj, spec, st, plugin, secrets, out)
	}

	if vol, err = p.store.CSIVolumeByID(nil, obj.Namespace, obj.Name); err == nil && vol != nil &&
		st.VolumeCreateIndex == vol.CreateIndex {
		volumeStatusFrom(st, vol)
	}
	o.writeRequest(p, obj, st, out, &finalizers)
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
// due yet.
func (o *volumeRequestOperator) retrying(p *volumeRequestPass, obj *structs.CustomObject, st *volumeRequestStatus, out *volumeRequestOutcome) bool {
	retryAt, err := time.Parse(time.RFC3339, st.RetryAt)
	if err != nil || !p.now.Before(retryAt) {
		return false
	}
	out.failed = volumeRequestFailure(obj.Status)
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

// make creates or registers the volume of a request.
func (o *volumeRequestOperator) make(p *volumeRequestPass, obj *structs.CustomObject, spec *volumeRequestSpec,
	st *volumeRequestStatus, plugin *structs.CSIPlugin, secrets structs.CSISecrets, out *volumeRequestOutcome) {

	if !pluginReady(spec, plugin, out) {
		p.want(p.now.Add(operatorsRetry))
		return
	}
	if o.retrying(p, obj, st, out) {
		return
	}
	vol := spec.volume(obj, secrets)
	if err := pluginSupportsAccessModes(plugin, vol); err != nil {
		out.notReady("Unsupported", "%v", err)
		return
	}
	vol.Provider, vol.ProviderVersion = plugin.Provider, plugin.Version

	what := "create the volume"
	if spec.mode() == volumeRequestModeRegister {
		what = "register the volume"
		if err := o.csi.validate(vol, plugin); err != nil {
			o.failure(p, obj, st, out, what, err)
			return
		}
	} else if err := o.csi.create(vol, plugin); err != nil {
		o.failure(p, obj, st, out, what, err)
		return
	} else if other, err := o.registeredElsewhere(vol); err != nil {
		o.failure(p, obj, st, out, what, err)
		return
	} else if other != nil {
		// The plugin answered a create of a name it knows with the volume
		// it has under that name. It is somebody else's: not registered a
		// second time, and never deleted with this request.
		o.failure(p, obj, st, out, what, fmt.Errorf("the plugin answered the name %q with the volume %q of the storage, "+
			"which is registered as %s/%s: the request needs a display_name of its own",
			vol.Name, vol.ExternalID, other.Namespace, other.ID))
		return
	}

	index, err := o.register(vol)
	if err != nil {
		o.failure(p, obj, st, out, what, err)
		return
	}
	st.VolumeCreateIndex, st.Creating = index, false
	st.Failures, st.RetryAt = 0, ""
	reason := "Created"
	if spec.mode() == volumeRequestModeRegister {
		reason = "Registered"
	}
	operatorCount(csiVolumeRequestKind, "volumes_"+strings.ToLower(reason))
	o.srv.operatorEvent(obj, structs.ObjectEventTypeNormal, reason,
		fmt.Sprintf("the volume %s (external ID %s) with the plugin %s", vol.ID, vol.ExternalID, vol.PluginID))
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
// returns its create index.
func (o *volumeRequestOperator) register(vol *structs.CSIVolume) (uint64, error) {
	req := &structs.CSIVolumeRegisterRequest{
		Volumes:      []*structs.CSIVolume{vol},
		WriteRequest: structs.WriteRequest{Region: o.srv.Region(), Namespace: vol.Namespace},
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
		st.Failures, st.RetryAt = 0, ""
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
	if o.retrying(p, obj, st, out) {
		return
	}

	// A change of the spec is an update; otherwise the volume was changed
	// through the volume API, and is set back.
	updated := adopted || obj.Status == nil || uint64(statusGeneration(obj.Status)) != obj.Generation
	changed := vol.Copy()

	// Merge refuses changes of what a volume can't change, and takes the
	// rest. The name of an adopted volume stays its own, and the context of
	// a created volume is the plugin's.
	mergeIn := *want
	mergeIn.Name = ""
	if spec.mode() == volumeRequestModeCreate {
		mergeIn.Context = maps.Clone(vol.Context)
	}
	// Merge refuses a change of the mount options of a volume in use, and
	// takes none (nil) and empty ones for a change: the stored volume has
	// empty ones where the request has none.
	if mountOptionsEqual(changed.MountOptions, mergeIn.MountOptions) {
		mergeIn.MountOptions = changed.MountOptions
	}
	if err := changed.Merge(&mergeIn); err != nil {
		o.failure(p, obj, st, out, "update the volume", err)
		return
	}
	changed.Secrets = maps.Clone(want.Secrets)
	if spec.mode() == volumeRequestModeRegister {
		changed.Context = maps.Clone(want.Context)
	}
	if err := o.csi.expand(changed, plugin, &csi.CapacityRange{
		RequiredBytes: want.RequestedCapacityMin,
		LimitBytes:    want.RequestedCapacityMax,
	}); err != nil {
		o.failure(p, obj, st, out, "expand the volume", err)
		return
	}
	// The plugin may refuse nothing and change nothing (no expansion
	// needed): the requested values are the spec's either way.
	changed.RequestedCapacityMin, changed.RequestedCapacityMax = want.RequestedCapacityMin, want.RequestedCapacityMax

	if _, err := o.register(changed); err != nil {
		o.failure(p, obj, st, out, "update the volume", err)
		return
	}
	st.Failures, st.RetryAt = 0, ""
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

// writeRequest writes the status of a request with its conditions.
func (o *volumeRequestOperator) writeRequest(p *volumeRequestPass, obj *structs.CustomObject, st *volumeRequestStatus,
	out *volumeRequestOutcome, finalizers *[]string) {

	encoded, err := structs.EncodeObjectPart(st)
	if err != nil {
		o.logger.Warn("status of a volume request can't be encoded", "request", obj.Name, "error", err)
		return
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
	conditions.set(structs.CertificateConditionDegraded, !ready && out.notReadyReason != "Creating", "", "")
	encoded[structs.ObjectStatusConditions] = conditions.list()
	encoded[structs.ObjectStatusObservedGeneration] = obj.Generation

	normalized, err := structs.NormalizeObjectMap(encoded)
	if err != nil {
		o.logger.Warn("status of a volume request can't be encoded", "request", obj.Name, "error", err)
		return
	}
	if structs.ObjectMapsEqual(obj.Status, normalized) && (finalizers == nil || slices.Equal(obj.Finalizers, *finalizers)) {
		return
	}
	changed := obj.Copy()
	changed.Status = normalized
	if finalizers != nil {
		changed.Finalizers = slices.Clone(*finalizers)
	}
	if _, err := o.srv.writeOperatorObject(changed); err != nil && !structs.IsObjectConflictError(err) {
		o.logger.Warn("failed to write the status of a volume request", "request", obj.Name,
			"namespace", obj.Namespace, "error", err)
	}
}

// finishDeletion does what the deletion of a request asks of the operator:
// with "deregister" the volume leaves Norca, with "delete" it is deleted in
// the storage once no allocation uses it, with "retain" it stays. A volume
// the request does not own is never touched.
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
