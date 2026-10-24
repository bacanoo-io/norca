// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"net/http"
	"strings"
	"time"

	"github.com/hashicorp/nomad/nomad/structs"
)

// backupIdentityHeader carries the age identity of GET browse requests. A
// request body (POST) is preferred; header values can't hold the comments
// of an identity file, so several identities are separated by commas.
const backupIdentityHeader = "X-Norca-Age-Identity"

// BackupsRequest handles /v1/operator/backups.
func (s *HTTPServer) BackupsRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	switch req.Method {
	case http.MethodGet:
		return s.backupList(resp, req)
	case http.MethodPut, http.MethodPost:
		return s.backupSave(resp, req)
	default:
		return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
	}
}

// BackupSpecificRequest handles /v1/operator/backup/:id and its
// sub-resources.
func (s *HTTPServer) BackupSpecificRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	path := strings.TrimPrefix(req.URL.Path, "/v1/operator/backup/")
	if path == "" {
		return nil, CodedError(http.StatusBadRequest, "missing backup ID")
	}

	// The restore records and the two-person approval of full restores.
	if path == "restores" {
		if req.Method != http.MethodGet {
			return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
		}
		return s.backupRestoreList(resp, req)
	}
	if rest, ok := strings.CutPrefix(path, "restore/"); ok {
		return s.backupRestoreRequest(resp, req, rest)
	}

	id, sub, _ := strings.Cut(path, "/")
	switch sub {
	case "":
		switch req.Method {
		case http.MethodGet:
			return s.backupQuery(resp, req, id)
		case http.MethodPut, http.MethodPost:
			return s.backupUpdate(resp, req, id)
		case http.MethodDelete:
			return s.backupDelete(resp, req, id)
		}
	case "contents":
		if req.Method == http.MethodGet || req.Method == http.MethodPost || req.Method == http.MethodPut {
			return s.backupContents(resp, req, id)
		}
	case "diff":
		if req.Method == http.MethodGet || req.Method == http.MethodPost || req.Method == http.MethodPut {
			return s.backupDiff(resp, req, id)
		}
	case "restore":
		if req.Method == http.MethodPost || req.Method == http.MethodPut {
			return s.backupRestore(resp, req, id)
		}
	case "download-url":
		if req.Method == http.MethodGet {
			return s.backupDownloadURL(resp, req, id)
		}
	default:
		return nil, CodedError(http.StatusNotFound, "unknown backup resource")
	}
	return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
}

func (s *HTTPServer) backupList(resp http.ResponseWriter, req *http.Request) (any, error) {
	args := structs.BackupListRequest{}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}

	var out structs.BackupListResponse
	if err := s.agent.RPC("Backup.List", &args, &out); err != nil {
		return nil, err
	}

	setMeta(resp, &out.QueryMeta)
	if out.Backups == nil {
		out.Backups = make([]*structs.BackupRecord, 0)
	}
	return &struct {
		Backups  []*structs.BackupRecord
		Schedule *structs.BackupScheduleStatus
	}{out.Backups, out.Schedule}, nil
}

func (s *HTTPServer) backupQuery(resp http.ResponseWriter, req *http.Request, id string) (any, error) {
	args := structs.BackupSpecificRequest{ID: id}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}

	var out structs.SingleBackupResponse
	if err := s.agent.RPC("Backup.Get", &args, &out); err != nil {
		return nil, err
	}

	setMeta(resp, &out.QueryMeta)
	if out.Backup == nil {
		return nil, CodedError(http.StatusNotFound, "backup not found")
	}
	return out.Backup, nil
}

func (s *HTTPServer) backupSave(resp http.ResponseWriter, req *http.Request) (any, error) {
	var args structs.BackupSaveRequest
	if req.ContentLength != 0 {
		if err := decodeBody(req, &args); err != nil {
			return nil, CodedError(http.StatusBadRequest, err.Error())
		}
	}
	s.parseWriteRequest(req, &args.WriteRequest)

	var out structs.BackupSaveResponse
	if err := s.agent.RPC("Backup.Save", &args, &out); err != nil {
		return nil, err
	}
	setIndex(resp, out.Index)
	return &struct {
		ID     string
		Backup *structs.BackupRecord
	}{out.ID, out.Backup}, nil
}

func (s *HTTPServer) backupUpdate(resp http.ResponseWriter, req *http.Request, id string) (any, error) {
	var args structs.BackupUpdateRequest
	if err := decodeBody(req, &args); err != nil {
		return nil, CodedError(http.StatusBadRequest, err.Error())
	}
	args.ID = id
	s.parseWriteRequest(req, &args.WriteRequest)

	var out structs.BackupUpdateResponse
	if err := s.agent.RPC("Backup.Update", &args, &out); err != nil {
		return nil, err
	}
	setIndex(resp, out.Index)
	return out.Backup, nil
}

func (s *HTTPServer) backupDelete(resp http.ResponseWriter, req *http.Request, id string) (any, error) {
	args := structs.BackupDeleteRequest{IDs: []string{id}}
	s.parseWriteRequest(req, &args.WriteRequest)

	var out structs.GenericResponse
	if err := s.agent.RPC("Backup.Delete", &args, &out); err != nil {
		return nil, err
	}
	setIndex(resp, out.Index)
	return nil, nil
}

// backupIdentity returns the age identity of a browse request: from the
// JSON body of a POST, or from the header of a GET.
func backupIdentity(req *http.Request) (string, error) {
	if req.Method == http.MethodGet {
		return strings.ReplaceAll(req.Header.Get(backupIdentityHeader), ",", "\n"), nil
	}
	var body struct {
		AgeIdentity string
	}
	if req.ContentLength != 0 {
		if err := decodeBody(req, &body); err != nil {
			return "", CodedError(http.StatusBadRequest, err.Error())
		}
	}
	return body.AgeIdentity, nil
}

func (s *HTTPServer) backupContents(resp http.ResponseWriter, req *http.Request, id string) (any, error) {
	identity, err := backupIdentity(req)
	if err != nil {
		return nil, err
	}
	q := req.URL.Query()
	args := structs.BackupContentsRequest{
		ID:          id,
		Type:        q.Get("type"),
		Destination: q.Get("destination"),
		JobID:       q.Get("job"),
		PathPrefix:  q.Get("prefix"),
		AgeIdentity: identity,
	}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}
	// The prefix query parameter filters the content, not the backup IDs.
	args.QueryOptions.Prefix = ""

	var out structs.BackupContentsResponse
	if err := s.agent.RPC("Backup.Contents", &args, &out); err != nil {
		return nil, err
	}
	setMeta(resp, &out.QueryMeta)
	return out.Contents, nil
}

func (s *HTTPServer) backupDiff(resp http.ResponseWriter, req *http.Request, id string) (any, error) {
	identity, err := backupIdentity(req)
	if err != nil {
		return nil, err
	}
	q := req.URL.Query()
	args := structs.BackupDiffRequest{
		ID:          id,
		Against:     q.Get("against"),
		Type:        q.Get("type"),
		JobID:       q.Get("job"),
		Destination: q.Get("destination"),
		AgeIdentity: identity,
	}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}

	var out structs.BackupDiffResponse
	if err := s.agent.RPC("Backup.Diff", &args, &out); err != nil {
		return nil, err
	}
	setMeta(resp, &out.QueryMeta)
	return out.Diff, nil
}

func (s *HTTPServer) backupRestore(resp http.ResponseWriter, req *http.Request, id string) (any, error) {
	var args structs.BackupRestoreRequest
	if err := decodeBody(req, &args); err != nil {
		return nil, CodedError(http.StatusBadRequest, err.Error())
	}
	args.ID = id
	s.parseWriteRequest(req, &args.WriteRequest)

	var out structs.BackupRestoreResponse
	if err := s.agent.RPC("Backup.Restore", &args, &out); err != nil {
		return nil, err
	}
	setIndex(resp, out.Index)
	return &struct {
		Restore *structs.BackupRestore
		Results []*structs.BackupRestoreItemResult
		Preview *structs.BackupRestorePreview
	}{out.Restore, out.Results, out.Preview}, nil
}

func (s *HTTPServer) backupDownloadURL(resp http.ResponseWriter, req *http.Request, id string) (any, error) {
	q := req.URL.Query()
	args := structs.BackupDownloadURLRequest{
		ID:          id,
		Destination: q.Get("destination"),
		Object:      q.Get("object"),
	}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}

	var out structs.BackupDownloadURLResponse
	if err := s.agent.RPC("Backup.DownloadURL", &args, &out); err != nil {
		return nil, err
	}
	setMeta(resp, &out.QueryMeta)
	return &struct {
		URL       string
		ExpiresAt time.Time
	}{out.URL, out.ExpiresAt}, nil
}

// backupRestoreRequest handles /v1/operator/backup/restore/:restore_id and
// its approve and reject actions.
func (s *HTTPServer) backupRestoreRequest(resp http.ResponseWriter, req *http.Request, path string) (any, error) {
	id, action, _ := strings.Cut(path, "/")
	if id == "" {
		return nil, CodedError(http.StatusBadRequest, "missing restore ID")
	}
	switch action {
	case "":
		if req.Method == http.MethodGet {
			return s.backupRestoreQuery(resp, req, id)
		}
	case "approve":
		if req.Method == http.MethodPost || req.Method == http.MethodPut {
			return s.backupRestoreApprove(resp, req, id)
		}
	case "reject":
		if req.Method == http.MethodPost || req.Method == http.MethodPut {
			return s.backupRestoreReject(resp, req, id)
		}
	default:
		return nil, CodedError(http.StatusNotFound, "unknown restore resource")
	}
	return nil, CodedError(http.StatusMethodNotAllowed, ErrInvalidMethod)
}

func (s *HTTPServer) backupRestoreList(resp http.ResponseWriter, req *http.Request) (any, error) {
	args := structs.BackupRestoreListRequest{}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}

	var out structs.BackupRestoreListResponse
	if err := s.agent.RPC("Backup.ListRestores", &args, &out); err != nil {
		return nil, err
	}
	setMeta(resp, &out.QueryMeta)
	if out.Restores == nil {
		out.Restores = make([]*structs.BackupRestore, 0)
	}
	return out.Restores, nil
}

func (s *HTTPServer) backupRestoreQuery(resp http.ResponseWriter, req *http.Request, id string) (any, error) {
	args := structs.BackupRestoreSpecificRequest{RestoreID: id}
	if s.parse(resp, req, &args.Region, &args.QueryOptions) {
		return nil, nil
	}

	var out structs.SingleBackupRestoreResponse
	if err := s.agent.RPC("Backup.GetRestore", &args, &out); err != nil {
		return nil, err
	}
	setMeta(resp, &out.QueryMeta)
	if out.Restore == nil {
		return nil, CodedError(http.StatusNotFound, "restore not found")
	}
	return out.Restore, nil
}

func (s *HTTPServer) backupRestoreApprove(resp http.ResponseWriter, req *http.Request, id string) (any, error) {
	var args structs.BackupRestoreApproveRequest
	if req.ContentLength != 0 {
		if err := decodeBody(req, &args); err != nil {
			return nil, CodedError(http.StatusBadRequest, err.Error())
		}
	}
	args.RestoreID = id
	s.parseWriteRequest(req, &args.WriteRequest)

	var out structs.BackupRestoreResponse
	if err := s.agent.RPC("Backup.ApproveRestore", &args, &out); err != nil {
		return nil, err
	}
	setIndex(resp, out.Index)
	return out.Restore, nil
}

func (s *HTTPServer) backupRestoreReject(resp http.ResponseWriter, req *http.Request, id string) (any, error) {
	var args structs.BackupRestoreRejectRequest
	if req.ContentLength != 0 {
		if err := decodeBody(req, &args); err != nil {
			return nil, CodedError(http.StatusBadRequest, err.Error())
		}
	}
	args.RestoreID = id
	s.parseWriteRequest(req, &args.WriteRequest)

	var out structs.BackupRestoreResponse
	if err := s.agent.RPC("Backup.RejectRestore", &args, &out); err != nil {
		return nil, err
	}
	setIndex(resp, out.Index)
	return out.Restore, nil
}

// keyringRestoreRequest adds the root keys of a backup to the keystore of
// every server of the region (Keyring.Restore).
func (s *HTTPServer) keyringRestoreRequest(resp http.ResponseWriter, req *http.Request) (any, error) {
	var args structs.KeyringRestoreRequest
	if err := decodeBody(req, &args); err != nil {
		return nil, CodedError(http.StatusBadRequest, err.Error())
	}
	// Only servers fan the keys out to a single server.
	args.LocalOnly = false
	s.parseWriteRequest(req, &args.WriteRequest)

	var out structs.KeyringRestoreResponse
	if err := s.agent.RPC("Keyring.Restore", &args, &out); err != nil {
		return nil, err
	}
	setIndex(resp, out.Index)
	return &struct {
		Added   []string
		Servers []string
		Errors  map[string]string
	}{out.Added, out.Servers, out.Errors}, nil
}
