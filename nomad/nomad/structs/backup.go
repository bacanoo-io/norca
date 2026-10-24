// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"fmt"
	"slices"
	"time"
)

const (
	// TopicBackup is the event stream topic of scheduled and manual backups
	// and of restores from them.
	TopicBackup Topic = "Backup"

	TypeBackupCompleted        = "BackupCompleted"
	TypeBackupFailed           = "BackupFailed"
	TypeBackupUpdated          = "BackupUpdated"
	TypeBackupDeleted          = "BackupDeleted"
	TypeBackupRestoreRequested = "BackupRestoreRequested"
	TypeBackupRestoreApproved  = "BackupRestoreApproved"
	TypeBackupRestoreStarted   = "BackupRestoreStarted"
	TypeBackupRestoreFinished  = "BackupRestoreFinished"
)

const (
	// BackupTriggerSchedule and BackupTriggerManual tell what started a
	// backup. BackupTriggerPreRestore is reserved for the automatic backup
	// taken before a full restore.
	BackupTriggerSchedule   = "schedule"
	BackupTriggerManual     = "manual"
	BackupTriggerPreRestore = "pre-restore"

	// BackupStatusComplete means the backup was verified on every
	// destination, BackupStatusPartial on at least one of them, and
	// BackupStatusFailed on none.
	BackupStatusComplete = "complete"
	BackupStatusPartial  = "partial"
	BackupStatusFailed   = "failed"

	// Statuses of a backup on a single destination.
	BackupDestinationStatusVerified = "verified"
	BackupDestinationStatusFailed   = "failed"

	// Names of the objects of a backup.
	BackupObjectState    = "state"
	BackupObjectKeyring  = "keyring"
	BackupObjectManifest = "manifest"
)

// Content types of a backup that can be browsed and compared.
const (
	BackupContentNamespaces  = "namespaces"
	BackupContentJobs        = "jobs"
	BackupContentJobVersions = "job-versions"
	BackupContentVariables   = "variables"
	BackupContentACLPolicies = "acl-policies"
	BackupContentACLRoles    = "acl-roles"
	BackupContentDeployments = "deployments"
	BackupContentAllocations = "allocations"
)

// BackupContentTypes are the content types in display order.
var BackupContentTypes = []string{
	BackupContentNamespaces,
	BackupContentJobs,
	BackupContentJobVersions,
	BackupContentVariables,
	BackupContentACLPolicies,
	BackupContentACLRoles,
	BackupContentDeployments,
	BackupContentAllocations,
}

// Restore modes, statuses and item types.
const (
	BackupRestoreModeSelective = "selective"
	BackupRestoreModeFull      = "full"

	// BackupRestoreStatusPending and BackupRestoreStatusApproved are used by
	// restores that need a second approval (full restore).
	BackupRestoreStatusPending  = "pending"
	BackupRestoreStatusApproved = "approved"
	BackupRestoreStatusRunning  = "running"
	BackupRestoreStatusComplete = "complete"
	BackupRestoreStatusFailed   = "failed"

	BackupRestoreItemJob       = "job"
	BackupRestoreItemVariable  = "variable"
	BackupRestoreItemNamespace = "namespace"
	BackupRestoreItemACLPolicy = "acl-policy"
	BackupRestoreItemACLRole   = "acl-role"
)

// BackupRecord describes a backup of the Raft state (and the keyring) of a
// region, stored in one or more S3 destinations. The records are kept in the
// backup_status table, so every server and the UI can list the backups
// without reaching the stores.
type BackupRecord struct {
	// ID is the time of the backup and its Raft index, for example
	// 20261024T140000Z-1834567.
	ID string

	Region              string
	AuthoritativeRegion string

	// Trigger is "schedule", "manual" or "pre-restore".
	Trigger string

	// Status is "complete", "partial" or "failed".
	Status            string
	StatusDescription string

	// StartedAt is when the backup started, CompletedAt when its last object
	// was verified (or it failed).
	StartedAt   time.Time
	CompletedAt time.Time

	// Versions of the leader that took the backup.
	NorcaVersion string
	NomadVersion string

	RaftIndex uint64
	RaftTerm  uint64
	LeaderID  string

	// Path is the location of the backup objects relative to the prefix of
	// each destination, for example 2026/10/24/20261024T140000Z-1834567.
	Path string

	// Destinations is the outcome per destination.
	Destinations []*BackupDestinationStatus

	// KeyIDs are the IDs of the root keys in the keyring object.
	KeyIDs []string

	// Summary holds object counts only, never names or content.
	Summary map[string]int

	Labels map[string]string
	Pinned bool

	CreateIndex uint64
	ModifyIndex uint64
}

// BackupDestinationStatus is the state of a backup on one destination.
type BackupDestinationStatus struct {
	// Name of the destination in the server configuration.
	Name string

	// Status is "verified" or "failed".
	Status string
	Error  string

	// Location is the URL of the backup folder, for example
	// s3://bucket/nomad/eu/2026/10/24/20261024T140000Z-1834567/.
	Location string

	// EncryptionMode and RecipientsFingerprint describe the client-side
	// encryption of the objects on this destination.
	EncryptionMode        string
	RecipientsFingerprint string

	// Objects are the uploaded objects by name ("state", "keyring").
	Objects map[string]*BackupObject

	Duration time.Duration
}

// BackupObject is an encrypted object of a backup.
type BackupObject struct {
	// Key is the object key relative to the backup folder.
	Key string

	// Size and SHA256 are those of the stored (encrypted) object.
	Size   int64
	SHA256 string
}

// Usable reports whether the backup can be browsed and restored.
func (b *BackupRecord) Usable() bool {
	return b != nil && (b.Status == BackupStatusComplete || b.Status == BackupStatusPartial)
}

// Destination returns the status of the named destination, or nil.
func (b *BackupRecord) Destination(name string) *BackupDestinationStatus {
	for _, d := range b.Destinations {
		if d.Name == name {
			return d
		}
	}
	return nil
}

// VerifiedDestinations returns the names of the destinations the backup was
// verified on.
func (b *BackupRecord) VerifiedDestinations() []string {
	var out []string
	for _, d := range b.Destinations {
		if d.Status == BackupDestinationStatusVerified {
			out = append(out, d.Name)
		}
	}
	return out
}

// Copy returns a deep copy of the record.
func (b *BackupRecord) Copy() *BackupRecord {
	if b == nil {
		return nil
	}
	nb := *b
	nb.Destinations = make([]*BackupDestinationStatus, 0, len(b.Destinations))
	for _, d := range b.Destinations {
		nd := *d
		if d.Objects != nil {
			nd.Objects = make(map[string]*BackupObject, len(d.Objects))
			for k, o := range d.Objects {
				no := *o
				nd.Objects[k] = &no
			}
		}
		nb.Destinations = append(nb.Destinations, &nd)
	}
	nb.KeyIDs = slices.Clone(b.KeyIDs)
	if b.Summary != nil {
		nb.Summary = make(map[string]int, len(b.Summary))
		for k, v := range b.Summary {
			nb.Summary[k] = v
		}
	}
	if b.Labels != nil {
		nb.Labels = make(map[string]string, len(b.Labels))
		for k, v := range b.Labels {
			nb.Labels[k] = v
		}
	}
	return &nb
}

// GetID implements the IDGetter interface required for pagination.
func (b *BackupRecord) GetID() string {
	return b.ID
}

const (
	maxBackupLabels           = 32
	maxBackupLabelKeyLength   = 128
	maxBackupLabelValueLength = 1024
)

// ValidateBackupLabels checks the labels of a backup.
func ValidateBackupLabels(labels map[string]string) error {
	if len(labels) > maxBackupLabels {
		return fmt.Errorf("at most %d labels are allowed", maxBackupLabels)
	}
	for k, v := range labels {
		if k == "" || len(k) > maxBackupLabelKeyLength {
			return fmt.Errorf("label key %q must be 1 to %d characters", k, maxBackupLabelKeyLength)
		}
		if len(v) > maxBackupLabelValueLength {
			return fmt.Errorf("label %q is longer than %d characters", k, maxBackupLabelValueLength)
		}
	}
	return nil
}

// BackupScheduleStatus is the state of the backup schedule of a region.
type BackupScheduleStatus struct {
	// Enabled reports whether scheduled backups are enabled.
	Enabled bool

	// Interval or Schedule (cron) of the scheduled backups.
	Interval time.Duration
	Schedule string

	// Destinations are the names of the configured destinations.
	Destinations []string

	LastSuccess   time.Time
	LastSuccessID string
	LastAttempt   time.Time
	LastError     string
	NextDue       time.Time

	// Overdue is set when the last successful backup is older than two
	// intervals.
	Overdue bool
}

// BackupUpsertRequest is the Raft request of creating or updating backup
// records.
type BackupUpsertRequest struct {
	Backups []*BackupRecord
	WriteRequest
}

// BackupDeleteRequest deletes backups: over RPC the backup objects and the
// record, in Raft the records only.
type BackupDeleteRequest struct {
	IDs []string
	WriteRequest
}

// BackupListRequest lists the backups of a region.
type BackupListRequest struct {
	QueryOptions
}

// BackupListResponse is the list of backups, newest first, with the state of
// the schedule.
type BackupListResponse struct {
	Backups  []*BackupRecord
	Schedule *BackupScheduleStatus
	QueryMeta
}

// BackupSpecificRequest addresses a single backup.
type BackupSpecificRequest struct {
	ID string
	QueryOptions
}

// SingleBackupResponse returns a single backup.
type SingleBackupResponse struct {
	Backup *BackupRecord
	QueryMeta
}

// BackupSaveRequest starts a manual backup.
type BackupSaveRequest struct {
	Labels map[string]string
	Pin    bool

	// Detach returns as soon as the snapshot is taken, with the ID of the
	// backup, instead of waiting for the upload.
	Detach bool
	WriteRequest
}

// BackupSaveResponse is the result of a manual backup.
type BackupSaveResponse struct {
	ID string

	// Backup is the finished backup; nil for a detached request.
	Backup *BackupRecord
	WriteMeta
}

// BackupUpdateRequest pins, unpins or labels a backup.
type BackupUpdateRequest struct {
	ID     string
	Pinned *bool

	// Labels replace the labels of the backup when not nil. Use
	// RemoveLabels to remove single labels.
	Labels       map[string]string
	RemoveLabels []string
	WriteRequest
}

// BackupUpdateResponse returns the updated backup.
type BackupUpdateResponse struct {
	Backup *BackupRecord
	WriteMeta
}

// BackupContentsRequest browses the content of a backup. The backup is
// decrypted with AgeIdentity, which is never stored or logged.
type BackupContentsRequest struct {
	ID string

	// Type is one of BackupContentTypes.
	Type string

	// Destination selects the store to read from; empty picks the first
	// verified one.
	Destination string

	// JobID filters job versions, deployments and allocations of a job.
	JobID string

	// PathPrefix filters Variables by path, and other objects by ID.
	PathPrefix string

	// AgeIdentity is the age private key (identity file content).
	AgeIdentity string

	QueryOptions
}

// BackupContentsResponse returns the content of a backup.
type BackupContentsResponse struct {
	Contents *BackupContents
	QueryMeta
}

// BackupContents is the content of a backup of a single type. Variables
// values are never returned.
type BackupContents struct {
	BackupID  string
	RaftIndex uint64
	Type      string

	Namespaces  []*Namespace
	Jobs        []*BackupJobStub
	JobVersions []*Job
	Variables   []*BackupVariableStub
	ACLPolicies []*ACLPolicyListStub
	ACLRoles    []*ACLRoleListStub
	Deployments []*Deployment
	Allocations []*AllocListStub
}

// BackupJobStub summarizes a job and its versions in a backup.
type BackupJobStub struct {
	ID             string
	Namespace      string
	Name           string
	Type           string
	Status         string
	Stop           bool
	Version        uint64
	Versions       []uint64
	SubmitTime     int64
	JobModifyIndex uint64
	ModifyIndex    uint64
}

// BackupVariableStub is a Variables entry of a backup: its path and the
// names of its keys (when the keyring of the backup could be read).
type BackupVariableStub struct {
	Namespace   string
	Path        string
	Keys        []string
	CreateTime  int64
	ModifyTime  int64
	ModifyIndex uint64
}

// BackupDiffRequest compares a backup with the current state or another
// backup.
type BackupDiffRequest struct {
	ID string

	// Against is "current" (the default) or the ID of another backup.
	Against string

	// Type restricts the comparison to one of BackupContentTypes.
	Type string

	// JobID restricts the comparison to a job; the response then contains
	// the job diff between the two sides.
	JobID string

	Destination string
	AgeIdentity string
	QueryOptions
}

// BackupDiffResponse returns the comparison.
type BackupDiffResponse struct {
	Diff *BackupDiff
	QueryMeta
}

// BackupDiff lists the objects that differ between From (a backup) and To
// (the current state or a newer backup): "added" exists only in To,
// "deleted" only in From.
type BackupDiff struct {
	From      string
	To        string
	FromIndex uint64
	ToIndex   uint64
	Objects   []*BackupObjectDiff

	// JobDiff is the diff of the job from From to To, when the request
	// named a job that exists on both sides.
	JobDiff *JobDiff
}

const (
	BackupDiffAdded    = "added"
	BackupDiffDeleted  = "deleted"
	BackupDiffModified = "modified"
)

// BackupObjectDiff is an object that differs between the two sides.
type BackupObjectDiff struct {
	// Type is the content type (jobs, variables, ...).
	Type      string
	Change    string
	Namespace string
	ID        string

	FromVersion     *uint64
	ToVersion       *uint64
	FromModifyIndex uint64
	ToModifyIndex   uint64
	FromStatus      string
	ToStatus        string
}

// BackupRestoreItem selects an object to restore from a backup.
type BackupRestoreItem struct {
	// Type is "job", "variable", "namespace", "acl-policy" or "acl-role".
	Type string

	// Namespace of a job or a Variables entry.
	Namespace string

	// ID is the job ID, Variables path, namespace name, ACL policy name or
	// ACL role name.
	ID string

	// Version of the job in the backup; nil restores the version that was
	// current at the time of the backup.
	Version *uint64

	// CheckIndex is the check-and-set index of a Variables entry: 0 (the
	// default) restores the entry only if it does not exist, otherwise it
	// must be the current modify index of the entry.
	CheckIndex *uint64
}

func (i *BackupRestoreItem) String() string {
	switch i.Type {
	case BackupRestoreItemJob:
		if i.Version != nil {
			return fmt.Sprintf("job %s/%s@%d", i.Namespace, i.ID, *i.Version)
		}
		return fmt.Sprintf("job %s/%s", i.Namespace, i.ID)
	case BackupRestoreItemVariable:
		return fmt.Sprintf("variable %s/%s", i.Namespace, i.ID)
	default:
		return fmt.Sprintf("%s %s", i.Type, i.ID)
	}
}

// BackupRestoreRequest restores from a backup. Only the selective mode is
// implemented; Confirm and the approval flow are reserved for the full
// restore.
type BackupRestoreRequest struct {
	ID          string
	Mode        string
	Items       []*BackupRestoreItem
	DryRun      bool
	Confirm     string
	Destination string
	AgeIdentity string
	WriteRequest
}

// BackupRestoreResponse is the result (or the preview) of a restore.
type BackupRestoreResponse struct {
	// Restore is the restore record; nil for a dry run.
	Restore *BackupRestore
	Results []*BackupRestoreItemResult
	WriteMeta
}

// BackupRestoreItemResult is the outcome of restoring a single item.
type BackupRestoreItemResult struct {
	Item *BackupRestoreItem

	// Action is "create", "update" or "none".
	Action string

	// Description is a human readable description of the change.
	Description string

	// CurrentIndex is the modify index of the current object, if any; it is
	// the check index to use when restoring a Variables entry over an
	// existing one.
	CurrentIndex uint64

	// Index is the Raft index of the write; 0 for a dry run or an error.
	Index uint64
	Error string
}

// BackupRestore is the record of a restore. Selective restores are recorded
// for the audit trail; the full restore will use the same record for its
// two-person approval.
type BackupRestore struct {
	ID       string
	BackupID string
	Mode     string
	Items    []*BackupRestoreItem
	Status   string

	StatusDescription string
	Results           []*BackupRestoreItemResult

	// RequestedBy is the accessor ID (or the identity) of the requester.
	RequestedBy string

	// Approvals are the accessor IDs of the approvers (full restore).
	Approvals []string

	CreateTime  time.Time
	ModifyTime  time.Time
	CreateIndex uint64
	ModifyIndex uint64
}

// Copy returns a copy of the restore record; items and results are shared.
func (r *BackupRestore) Copy() *BackupRestore {
	if r == nil {
		return nil
	}
	nr := *r
	nr.Items = slices.Clone(r.Items)
	nr.Results = slices.Clone(r.Results)
	nr.Approvals = slices.Clone(r.Approvals)
	return &nr
}

// BackupRestoreUpsertRequest is the Raft request of creating or updating
// restore records.
type BackupRestoreUpsertRequest struct {
	Restores []*BackupRestore
	WriteRequest
}

// BackupDownloadURLRequest asks for a short-lived pre-signed URL of a backup
// object.
type BackupDownloadURLRequest struct {
	ID          string
	Destination string

	// Object is "state", "keyring" or "manifest".
	Object string
	QueryOptions
}

// BackupDownloadURLResponse returns the pre-signed URL.
type BackupDownloadURLResponse struct {
	URL       string
	ExpiresAt time.Time
	QueryMeta
}

// BackupEvent is the payload of a Backup topic event about a backup.
type BackupEvent struct {
	Backup *BackupRecord
}

// BackupRestoreEvent is the payload of a Backup topic event about a
// restore.
type BackupRestoreEvent struct {
	Restore *BackupRestore
}
