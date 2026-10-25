// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"errors"
	"net/url"
	"time"
)

const (
	// TopicBackup is the event stream topic of backups and restores.
	TopicBackup Topic = "Backup"

	BackupTriggerSchedule   = "schedule"
	BackupTriggerManual     = "manual"
	BackupTriggerPreRestore = "pre-restore"

	// Statuses of a restore.
	BackupRestoreStatusPending  = "pending"
	BackupRestoreStatusApproved = "approved"
	BackupRestoreStatusRejected = "rejected"
	BackupRestoreStatusExpired  = "expired"
	BackupRestoreStatusRunning  = "running"
	BackupRestoreStatusComplete = "complete"
	BackupRestoreStatusFailed   = "failed"

	BackupStatusComplete = "complete"
	BackupStatusPartial  = "partial"
	BackupStatusFailed   = "failed"

	// Content types of a backup.
	BackupContentNamespaces  = "namespaces"
	BackupContentJobs        = "jobs"
	BackupContentJobVersions = "job-versions"
	BackupContentVariables   = "variables"
	BackupContentACLPolicies = "acl-policies"
	BackupContentACLRoles    = "acl-roles"
	BackupContentDeployments = "deployments"
	BackupContentAllocations = "allocations"

	BackupRestoreModeSelective = "selective"
	BackupRestoreModeFull      = "full"

	// Restore item types.
	BackupRestoreItemJob       = "job"
	BackupRestoreItemVariable  = "variable"
	BackupRestoreItemNamespace = "namespace"
	BackupRestoreItemACLPolicy = "acl-policy"
	BackupRestoreItemACLRole   = "acl-role"

	// Backup objects.
	BackupObjectState    = "state"
	BackupObjectKeyring  = "keyring"
	BackupObjectManifest = "manifest"
)

// Backups is used to access the backup endpoints.
type Backups struct {
	client *Client
}

// Backups returns a handle on the backup endpoints.
func (c *Client) Backups() *Backups {
	return &Backups{client: c}
}

// Backup is a backup of the Raft state and keyring of a region.
type Backup struct {
	ID                  string
	Region              string
	AuthoritativeRegion string
	Trigger             string
	Status              string
	StatusDescription   string
	StartedAt           time.Time
	CompletedAt         time.Time
	NorcaVersion        string
	NomadVersion        string
	RaftIndex           uint64
	RaftTerm            uint64
	LeaderID            string
	Path                string
	Destinations        []*BackupDestinationStatus
	KeyIDs              []string
	Summary             map[string]int
	Labels              map[string]string
	Pinned              bool
	CreateIndex         uint64
	ModifyIndex         uint64
}

// BackupDestinationStatus is the state of a backup on a destination.
type BackupDestinationStatus struct {
	Name                  string
	Status                string
	Error                 string
	Location              string
	EncryptionMode        string
	RecipientsFingerprint string
	Objects               map[string]*BackupObject
	Duration              time.Duration
}

// BackupObject is an encrypted object of a backup.
type BackupObject struct {
	Key    string
	Size   int64
	SHA256 string
}

// BackupScheduleStatus is the state of the backup schedule of a region.
type BackupScheduleStatus struct {
	Enabled       bool
	Interval      time.Duration
	Schedule      string
	Destinations  []string
	LastSuccess   time.Time
	LastSuccessID string
	LastAttempt   time.Time
	LastError     string
	NextDue       time.Time
	Overdue       bool
}

// BackupList is the list of the backups of a region, newest first.
type BackupList struct {
	Backups  []*Backup
	Schedule *BackupScheduleStatus
}

// BackupSaveRequest starts a manual backup.
type BackupSaveRequest struct {
	Labels map[string]string
	Pin    bool

	// Detach returns once the snapshot is taken, with the backup ID only.
	Detach bool
}

// BackupSaveResponse is the result of a manual backup.
type BackupSaveResponse struct {
	ID     string
	Backup *Backup
}

// BackupUpdateRequest pins, unpins or labels a backup.
type BackupUpdateRequest struct {
	Pinned       *bool
	Labels       map[string]string
	RemoveLabels []string
}

// BackupContentsOptions select the content of a backup to browse.
type BackupContentsOptions struct {
	// Type is one of the BackupContent* types; jobs by default.
	Type        string
	JobID       string
	PathPrefix  string
	Destination string

	// AgeIdentity is the content of an age identity file. It is sent in the
	// request body and never stored.
	AgeIdentity string
}

// BackupContents is the content of a backup of a single type.
type BackupContents struct {
	BackupID    string
	RaftIndex   uint64
	Type        string
	Namespaces  []*Namespace
	Jobs        []*BackupJobStub
	JobVersions []*Job
	Variables   []*BackupVariableStub
	ACLPolicies []*ACLPolicyListStub
	ACLRoles    []*ACLRoleListStub
	Deployments []*Deployment
	Allocations []*AllocationListStub
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

// BackupVariableStub is a Variables entry of a backup, without values.
type BackupVariableStub struct {
	Namespace   string
	Path        string
	Keys        []string
	CreateTime  int64
	ModifyTime  int64
	ModifyIndex uint64
}

// BackupDiffOptions select a comparison.
type BackupDiffOptions struct {
	// Against is "current" (default) or the ID of another backup.
	Against     string
	Type        string
	JobID       string
	Destination string
	AgeIdentity string
}

// BackupDiff lists the objects that differ between a backup (From) and the
// current state or another backup (To).
type BackupDiff struct {
	From      string
	To        string
	FromIndex uint64
	ToIndex   uint64
	Objects   []*BackupObjectDiff
	JobDiff   *JobDiff
}

// BackupObjectDiff is an object that is "added", "deleted" or "modified".
type BackupObjectDiff struct {
	Type            string
	Change          string
	Namespace       string
	ID              string
	FromVersion     *uint64
	ToVersion       *uint64
	FromModifyIndex uint64
	ToModifyIndex   uint64
	FromStatus      string
	ToStatus        string
}

// BackupRestoreItem selects an object to restore.
type BackupRestoreItem struct {
	Type       string
	Namespace  string
	ID         string
	Version    *uint64
	CheckIndex *uint64
}

// BackupRestoreRequest restores items from a backup (selective mode), or
// requests a full restore of the region (full mode) that a second
// administrator approves. Confirm must be the name of the region for a full
// restore.
type BackupRestoreRequest struct {
	Mode        string
	Items       []*BackupRestoreItem
	DryRun      bool
	Confirm     string
	Destination string
	AgeIdentity string
}

// BackupRestoreResponse is the result or preview of a restore.
type BackupRestoreResponse struct {
	Restore *BackupRestore
	Results []*BackupRestoreItemResult
	Preview *BackupRestorePreview
}

// BackupRestorePreview summarizes what a full restore would change. Counts
// are keyed by "<content type>/<removed|restored|reverted>".
type BackupRestorePreview struct {
	BackupID           string
	Region             string
	BackupIndex        uint64
	CurrentIndex       uint64
	Counts             map[string]int
	AllocationsStopped int
	ACLTokensRemoved   int
	KeysAdded          []string
	Warnings           []string
	Objects            []*BackupObjectDiff
	Truncated          bool
}

// BackupRestoreApproveRequest approves a pending full restore, which then
// runs. The approver supplies the age identity of the backup.
type BackupRestoreApproveRequest struct {
	AgeIdentity string
}

// BackupRestoreRejectRequest rejects a pending full restore.
type BackupRestoreRejectRequest struct {
	Reason string
}

// RootKey is a root key of the keyring with its key material, as restored
// from a backup.
type RootKey struct {
	Meta *RootKeyMeta
	Key  []byte
}

// KeyringRestoreResponse lists the servers that hold the restored keys.
type KeyringRestoreResponse struct {
	Added   []string
	Servers []string
	Errors  map[string]string
}

// BackupRestoreItemResult is the outcome of restoring an item.
type BackupRestoreItemResult struct {
	Item         *BackupRestoreItem
	Action       string
	Description  string
	CurrentIndex uint64
	Index        uint64
	Error        string
}

// BackupRestore is the record of a restore.
type BackupRestore struct {
	ID                 string
	BackupID           string
	Mode               string
	Items              []*BackupRestoreItem
	Status             string
	StatusDescription  string
	Results            []*BackupRestoreItemResult
	RequestedBy        string
	RequestedByName    string
	Approvals          []string
	ApprovedAt         time.Time
	RejectedBy         string
	Destination        string
	ExpiresAt          time.Time
	PreRestoreBackupID string
	Preview            *BackupRestorePreview
	CreateTime         time.Time
	ModifyTime         time.Time
	CreateIndex        uint64
	ModifyIndex        uint64
}

// BackupDownloadURL is a short-lived pre-signed URL of a backup object.
type BackupDownloadURL struct {
	URL       string
	ExpiresAt time.Time
}

// BackupEvent is the payload of a Backup topic event about a backup.
type BackupEvent struct {
	Backup *Backup
}

// BackupRestoreEvent is the payload of a Backup topic event about a
// restore.
type BackupRestoreEvent struct {
	Restore *BackupRestore
}

func backupPath(id string, sub string) string {
	p := "/v1/operator/backup/" + url.PathEscape(id)
	if sub != "" {
		p += "/" + sub
	}
	return p
}

// withParams returns a copy of the query options with extra parameters.
func backupQueryParams(q *QueryOptions, params map[string]string) *QueryOptions {
	var nq QueryOptions
	if q != nil {
		nq = *q
	}
	merged := make(map[string]string, len(nq.Params)+len(params))
	for k, v := range nq.Params {
		merged[k] = v
	}
	for k, v := range params {
		if v != "" {
			merged[k] = v
		}
	}
	nq.Params = merged
	return &nq
}

// List returns the backups of the region and the state of the schedule.
func (b *Backups) List(q *QueryOptions) (*BackupList, *QueryMeta, error) {
	var resp BackupList
	qm, err := b.client.query("/v1/operator/backups", &resp, q)
	if err != nil {
		return nil, nil, err
	}
	return &resp, qm, nil
}

// Info returns a backup.
func (b *Backups) Info(id string, q *QueryOptions) (*Backup, *QueryMeta, error) {
	if id == "" {
		return nil, nil, errors.New("missing backup ID")
	}
	var resp Backup
	qm, err := b.client.query(backupPath(id, ""), &resp, q)
	if err != nil {
		return nil, nil, err
	}
	return &resp, qm, nil
}

// Save takes a manual backup on the leader. It waits for the upload unless
// the request is detached.
func (b *Backups) Save(req *BackupSaveRequest, w *WriteOptions) (*BackupSaveResponse, *WriteMeta, error) {
	if req == nil {
		req = &BackupSaveRequest{}
	}
	var resp BackupSaveResponse
	wm, err := b.client.post("/v1/operator/backups", req, &resp, w)
	if err != nil {
		return nil, nil, err
	}
	return &resp, wm, nil
}

// Update pins, unpins or labels a backup.
func (b *Backups) Update(id string, req *BackupUpdateRequest, w *WriteOptions) (*Backup, *WriteMeta, error) {
	if id == "" {
		return nil, nil, errors.New("missing backup ID")
	}
	var resp Backup
	wm, err := b.client.put(backupPath(id, ""), req, &resp, w)
	if err != nil {
		return nil, nil, err
	}
	return &resp, wm, nil
}

// Delete deletes a backup from its destinations. Pinned backups and the
// min_keep newest backups can't be deleted.
func (b *Backups) Delete(id string, w *WriteOptions) (*WriteMeta, error) {
	if id == "" {
		return nil, errors.New("missing backup ID")
	}
	return b.client.delete(backupPath(id, ""), nil, nil, w)
}

// Contents browses the content of a backup. The backup is decrypted on the
// server with the age identity of the options.
func (b *Backups) Contents(id string, opts *BackupContentsOptions, q *QueryOptions) (*BackupContents, *QueryMeta, error) {
	if id == "" {
		return nil, nil, errors.New("missing backup ID")
	}
	if opts == nil {
		opts = &BackupContentsOptions{}
	}
	q = backupQueryParams(q, map[string]string{
		"type": opts.Type, "job": opts.JobID, "prefix": opts.PathPrefix, "destination": opts.Destination,
	})
	body := struct{ AgeIdentity string }{opts.AgeIdentity}
	var resp BackupContents
	qm, err := b.client.postQuery(backupPath(id, "contents"), &body, &resp, q)
	if err != nil {
		return nil, nil, err
	}
	return &resp, qm, nil
}

// Diff compares a backup with the current state or another backup.
func (b *Backups) Diff(id string, opts *BackupDiffOptions, q *QueryOptions) (*BackupDiff, *QueryMeta, error) {
	if id == "" {
		return nil, nil, errors.New("missing backup ID")
	}
	if opts == nil {
		opts = &BackupDiffOptions{}
	}
	q = backupQueryParams(q, map[string]string{
		"against": opts.Against, "type": opts.Type, "job": opts.JobID, "destination": opts.Destination,
	})
	body := struct{ AgeIdentity string }{opts.AgeIdentity}
	var resp BackupDiff
	qm, err := b.client.postQuery(backupPath(id, "diff"), &body, &resp, q)
	if err != nil {
		return nil, nil, err
	}
	return &resp, qm, nil
}

// Restore restores items from a backup, or previews the restore with
// DryRun.
func (b *Backups) Restore(id string, req *BackupRestoreRequest, w *WriteOptions) (*BackupRestoreResponse, *WriteMeta, error) {
	if id == "" {
		return nil, nil, errors.New("missing backup ID")
	}
	if req == nil {
		return nil, nil, errors.New("missing restore request")
	}
	var resp BackupRestoreResponse
	wm, err := b.client.post(backupPath(id, "restore"), req, &resp, w)
	if err != nil {
		return nil, nil, err
	}
	return &resp, wm, nil
}

// DownloadURL returns a short-lived pre-signed download URL of a backup
// object ("state", "keyring" or "manifest").
func (b *Backups) DownloadURL(id, destination, object string, q *QueryOptions) (*BackupDownloadURL, *QueryMeta, error) {
	if id == "" {
		return nil, nil, errors.New("missing backup ID")
	}
	q = backupQueryParams(q, map[string]string{"destination": destination, "object": object})
	var resp BackupDownloadURL
	qm, err := b.client.query(backupPath(id, "download-url"), &resp, q)
	if err != nil {
		return nil, nil, err
	}
	return &resp, qm, nil
}

// GetRestore returns a restore record, with the preview of a full restore.
func (b *Backups) GetRestore(restoreID string, q *QueryOptions) (*BackupRestore, *QueryMeta, error) {
	if restoreID == "" {
		return nil, nil, errors.New("missing restore ID")
	}
	var resp BackupRestore
	qm, err := b.client.query("/v1/operator/backup/restore/"+url.PathEscape(restoreID), &resp, q)
	if err != nil {
		return nil, nil, err
	}
	return &resp, qm, nil
}

// ListRestores returns the restore records of the region, newest first.
func (b *Backups) ListRestores(q *QueryOptions) ([]*BackupRestore, *QueryMeta, error) {
	var resp []*BackupRestore
	qm, err := b.client.query("/v1/operator/backup/restores", &resp, q)
	if err != nil {
		return nil, nil, err
	}
	return resp, qm, nil
}

// ApproveRestore approves a pending full restore as a second administrator
// and waits for the restore to finish.
func (b *Backups) ApproveRestore(restoreID string, req *BackupRestoreApproveRequest, w *WriteOptions) (*BackupRestore, *WriteMeta, error) {
	if restoreID == "" {
		return nil, nil, errors.New("missing restore ID")
	}
	if req == nil {
		req = &BackupRestoreApproveRequest{}
	}
	var resp BackupRestore
	wm, err := b.client.post("/v1/operator/backup/restore/"+url.PathEscape(restoreID)+"/approve", req, &resp, w)
	if err != nil {
		return nil, nil, err
	}
	return &resp, wm, nil
}

// RejectRestore rejects (or withdraws) a pending full restore.
func (b *Backups) RejectRestore(restoreID string, req *BackupRestoreRejectRequest, w *WriteOptions) (*BackupRestore, *WriteMeta, error) {
	if restoreID == "" {
		return nil, nil, errors.New("missing restore ID")
	}
	if req == nil {
		req = &BackupRestoreRejectRequest{}
	}
	var resp BackupRestore
	wm, err := b.client.post("/v1/operator/backup/restore/"+url.PathEscape(restoreID)+"/reject", req, &resp, w)
	if err != nil {
		return nil, nil, err
	}
	return &resp, wm, nil
}

// Restore adds root keys of a backup to the keystore of every server of the
// region, alongside the existing keys. It is used before the Raft state of
// the backup is restored, for example when building a new cluster from a
// backup.
func (k *Keyring) Restore(keys []*RootKey, w *WriteOptions) (*KeyringRestoreResponse, *WriteMeta, error) {
	if len(keys) == 0 {
		return nil, nil, errors.New("no root keys to restore")
	}
	req := struct{ Keys []*RootKey }{keys}
	var resp KeyringRestoreResponse
	wm, err := k.client.put("/v1/operator/keyring/restore", &req, &resp, w)
	if err != nil {
		return nil, nil, err
	}
	return &resp, wm, nil
}
