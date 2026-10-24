// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"
)

var (
	// ErrNotFound is returned when an object, Variable or job does not exist.
	ErrNotFound = errors.New("not found")

	// ErrConflict is returned when a check-and-set write fails because the
	// stored object was modified since it was read.
	ErrConflict = errors.New("check-and-set conflict")
)

// VariablesAPI is the part of the Variables API the SDK uses.
type VariablesAPI interface {
	// ListVariables lists the metadata of the Variables under prefix. With a
	// positive waitIndex it is a blocking query that returns when the
	// Variables changed after waitIndex (or the wait time elapsed). It
	// returns the index of the listing.
	ListVariables(ctx context.Context, namespace, prefix string, waitIndex uint64) ([]*api.VariableMetadata, uint64, error)

	// ReadVariable reads a Variable. It returns ErrNotFound if the Variable
	// does not exist; a permission error is returned as an error, never as
	// ErrNotFound.
	ReadVariable(ctx context.Context, namespace, path string) (*api.Variable, error)

	// WriteVariable writes v with check-and-set on v.ModifyIndex (0: the
	// Variable must not exist). On a conflict it returns an error wrapping
	// ErrConflict.
	WriteVariable(ctx context.Context, v *api.Variable) (*api.Variable, error)

	// DeleteVariable deletes a Variable with check-and-set on modifyIndex,
	// or unconditionally if modifyIndex is 0. On a conflict it returns an
	// error wrapping ErrConflict. Deleting a missing Variable is not an
	// error.
	DeleteVariable(ctx context.Context, namespace, path string, modifyIndex uint64) error
}

// EventsAPI is the part of the event stream API the SDK uses.
type EventsAPI interface {
	// Stream subscribes to the event stream. With exact set, every event
	// from index on is delivered, or the call fails with an error wrapping
	// api.ErrEventIndexGone.
	Stream(ctx context.Context, namespace string, topics map[api.Topic][]string, index uint64, exact bool) (<-chan *api.Events, error)
}

// JobsAPI is the part of the Jobs API the SDK uses.
type JobsAPI interface {
	// ListJobs lists the jobs of a namespace including their meta. With a
	// positive waitIndex it is a blocking query.
	ListJobs(ctx context.Context, namespace string, waitIndex uint64) ([]*api.JobListStub, uint64, error)

	// ReadJob reads a job. It returns ErrNotFound if the job does not exist.
	ReadJob(ctx context.Context, namespace, id string) (*api.Job, error)

	// RegisterJob registers (creates or updates) a job.
	RegisterJob(ctx context.Context, job *api.Job) error

	// DeregisterJob stops a job, purging it if purge is set. A job that
	// does not exist is not an error.
	DeregisterJob(ctx context.Context, namespace, id string, purge bool) error
}

// ObjectsAPI is the part of the native custom objects API the SDK uses
// (objects of a registered ObjectKind, StorageNative).
type ObjectsAPI interface {
	// ListObjects lists the objects of the kind in the namespace ("*" for
	// all). With a positive waitIndex it is a blocking query that returns
	// when objects changed after waitIndex (or the wait time elapsed). It
	// returns the index of the listing.
	ListObjects(ctx context.Context, kind Kind, namespace string, waitIndex uint64) ([]*api.CustomObject, uint64, error)

	// ReadObject reads an object. It returns an error wrapping ErrNotFound
	// if the object does not exist.
	ReadObject(ctx context.Context, kind Kind, namespace, name string) (*api.CustomObject, error)

	// ApplyObject writes the spec and metadata of an object with
	// check-and-set on cas (0: the object must not exist); the status is
	// not changed. On a conflict it returns an error wrapping ErrConflict.
	// It returns the object as stored.
	ApplyObject(ctx context.Context, obj *api.CustomObject, cas uint64) (*api.CustomObject, error)

	// UpdateObjectStatus replaces the status of an object with
	// check-and-set on cas (0: unconditionally). On a conflict it returns
	// an error wrapping ErrConflict, and ErrNotFound if the object does not
	// exist. It returns the object as stored.
	UpdateObjectStatus(ctx context.Context, kind Kind, namespace, name string, status map[string]any, cas uint64) (*api.CustomObject, error)

	// DeleteObject deletes an object with check-and-set on cas, or
	// unconditionally if cas is 0. On a conflict it returns an error
	// wrapping ErrConflict. Deleting a missing object is not an error. An
	// object with finalizers is only marked for deletion.
	DeleteObject(ctx context.Context, kind Kind, namespace, name string, cas uint64) error

	// UpdateObjectFinalizers adds and removes finalizers of an object (the
	// "manage-finalizers" capability), with check-and-set on cas, or
	// unconditionally if cas is 0. It returns ErrNotFound if the object
	// does not exist. Removing the last finalizer of an object marked for
	// deletion deletes it. It returns the object as stored.
	UpdateObjectFinalizers(ctx context.Context, kind Kind, namespace, name string, add, remove []string, cas uint64) (*api.CustomObject, error)

	// RecordObjectEvents adds events to the event log of an object (the
	// "write-events" capability). It returns ErrNotFound if the object does
	// not exist.
	RecordObjectEvents(ctx context.Context, kind Kind, namespace, name string, events []*api.ObjectEvent) error

	// ListObjectEvents returns the event log of an object, oldest first.
	ListObjectEvents(ctx context.Context, kind Kind, namespace, name string) ([]*api.ObjectEvent, error)
}

// Backend is everything the SDK needs from Nomad.
type Backend interface {
	VariablesAPI
	ObjectsAPI
	EventsAPI
	JobsAPI
}

// NewBackend returns a Backend that uses the Nomad API client.
func NewBackend(client *api.Client) Backend {
	return &apiBackend{client: client}
}

type apiBackend struct {
	client *api.Client
}

// StatusError is an error with an HTTP status code. The API client returns
// api.UnexpectedResponseError; fakes return StatusError, which the SDK
// treats the same way.
type StatusError struct {
	Code    int
	Message string
}

// NewStatusError returns a StatusError.
func NewStatusError(code int, message string) error {
	return &StatusError{Code: code, Message: message}
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("Unexpected response code: %d (%s)", e.Code, e.Message)
}

// statusCode returns the HTTP status code of an API error, or 0.
func statusCode(err error) int {
	var respErr api.UnexpectedResponseError
	if errors.As(err, &respErr) {
		return respErr.StatusCode()
	}
	var statusErr *StatusError
	if errors.As(err, &statusErr) {
		return statusErr.Code
	}
	return 0
}

func (b *apiBackend) ListVariables(ctx context.Context, namespace, prefix string, waitIndex uint64) ([]*api.VariableMetadata, uint64, error) {
	q := (&api.QueryOptions{Namespace: namespace, Prefix: prefix, WaitIndex: waitIndex}).WithContext(ctx)
	vars, qm, err := b.client.Variables().List(q)
	if err != nil {
		return nil, 0, err
	}
	return vars, qm.LastIndex, nil
}

func (b *apiBackend) ReadVariable(ctx context.Context, namespace, path string) (*api.Variable, error) {
	// Variables.Read reports a permission error as an error wrapping
	// api.ErrVariablePermissionDenied, never as ErrVariablePathNotFound.
	q := (&api.QueryOptions{Namespace: namespace}).WithContext(ctx)
	v, _, err := b.client.Variables().Read(path, q)
	if err != nil {
		if errors.Is(err, api.ErrVariablePathNotFound) {
			return nil, fmt.Errorf("variable %s/%s: %w", namespace, path, ErrNotFound)
		}
		return nil, err
	}
	return v, nil
}

func (b *apiBackend) WriteVariable(ctx context.Context, v *api.Variable) (*api.Variable, error) {
	w := (&api.WriteOptions{Namespace: v.Namespace}).WithContext(ctx)
	var (
		out *api.Variable
		err error
	)
	if v.ModifyIndex == 0 {
		out, _, err = b.client.Variables().CheckedCreate(v, w)
	} else {
		out, _, err = b.client.Variables().CheckedUpdate(v, w)
	}
	var casErr api.ErrCASConflict
	if errors.As(err, &casErr) {
		return nil, fmt.Errorf("%w: %v", ErrConflict, casErr)
	}
	return out, err
}

func (b *apiBackend) DeleteVariable(ctx context.Context, namespace, path string, modifyIndex uint64) error {
	w := (&api.WriteOptions{Namespace: namespace}).WithContext(ctx)
	if modifyIndex == 0 {
		_, err := b.client.Variables().Delete(path, w)
		return err
	}
	_, err := b.client.Variables().CheckedDelete(path, modifyIndex, w)
	var casErr api.ErrCASConflict
	if errors.As(err, &casErr) {
		return fmt.Errorf("%w: %v", ErrConflict, casErr)
	}
	return err
}

func (b *apiBackend) ListObjects(ctx context.Context, kind Kind, namespace string, waitIndex uint64) ([]*api.CustomObject, uint64, error) {
	q := (&api.QueryOptions{Namespace: namespace, WaitIndex: waitIndex}).WithContext(ctx)
	objs, qm, err := b.client.Objects().List(kind.Group, kind.Kind, nil, q)
	if err != nil {
		return nil, 0, err
	}
	return objs, qm.LastIndex, nil
}

func (b *apiBackend) ReadObject(ctx context.Context, kind Kind, namespace, name string) (*api.CustomObject, error) {
	q := (&api.QueryOptions{Namespace: namespace}).WithContext(ctx)
	obj, _, err := b.client.Objects().Get(kind.Group, kind.Kind, name, q)
	if err != nil {
		if statusCode(err) == http.StatusNotFound {
			return nil, fmt.Errorf("%s %s/%s: %w", kind, namespace, name, ErrNotFound)
		}
		return nil, err
	}
	return obj, nil
}

// objectError maps the errors of object writes to the SDK errors.
func objectError(kind Kind, namespace, name string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, api.ErrObjectConflict):
		return fmt.Errorf("%w: %v", ErrConflict, err)
	case statusCode(err) == http.StatusNotFound:
		return fmt.Errorf("%s %s/%s: %w (%v)", kind, namespace, name, ErrNotFound, err)
	}
	return err
}

func (b *apiBackend) ApplyObject(ctx context.Context, obj *api.CustomObject, cas uint64) (*api.CustomObject, error) {
	w := (&api.WriteOptions{Namespace: obj.Namespace}).WithContext(ctx)
	out, _, err := b.client.Objects().Apply(obj, &api.ObjectWriteOptions{CAS: api.CASIndex(cas)}, w)
	if err != nil {
		return nil, objectError(Kind{Group: obj.Group, Kind: obj.Kind}, obj.Namespace, obj.Name, err)
	}
	return out, nil
}

func (b *apiBackend) UpdateObjectStatus(ctx context.Context, kind Kind, namespace, name string, status map[string]any, cas uint64) (*api.CustomObject, error) {
	w := (&api.WriteOptions{Namespace: namespace}).WithContext(ctx)
	opts := &api.ObjectWriteOptions{}
	if cas != 0 {
		opts.CAS = api.CASIndex(cas)
	}
	out, _, err := b.client.Objects().UpdateStatus(kind.Group, kind.Kind, name, status, opts, w)
	if err != nil {
		return nil, objectError(kind, namespace, name, err)
	}
	return out, nil
}

func (b *apiBackend) DeleteObject(ctx context.Context, kind Kind, namespace, name string, cas uint64) error {
	w := (&api.WriteOptions{Namespace: namespace}).WithContext(ctx)
	opts := &api.ObjectWriteOptions{}
	if cas != 0 {
		opts.CAS = api.CASIndex(cas)
	}
	_, _, err := b.client.Objects().Delete(kind.Group, kind.Kind, name, opts, w)
	err = objectError(kind, namespace, name, err)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (b *apiBackend) UpdateObjectFinalizers(ctx context.Context, kind Kind, namespace, name string, add, remove []string, cas uint64) (*api.CustomObject, error) {
	w := (&api.WriteOptions{Namespace: namespace}).WithContext(ctx)
	opts := &api.ObjectWriteOptions{}
	if cas != 0 {
		opts.CAS = api.CASIndex(cas)
	}
	out, _, err := b.client.Objects().UpdateFinalizers(kind.Group, kind.Kind, name, add, remove, opts, w)
	if err != nil {
		return nil, objectError(kind, namespace, name, err)
	}
	return out, nil
}

func (b *apiBackend) RecordObjectEvents(ctx context.Context, kind Kind, namespace, name string, events []*api.ObjectEvent) error {
	w := (&api.WriteOptions{Namespace: namespace}).WithContext(ctx)
	_, err := b.client.Objects().RecordEvents(kind.Group, kind.Kind, name, events, w)
	return objectError(kind, namespace, name, err)
}

func (b *apiBackend) ListObjectEvents(ctx context.Context, kind Kind, namespace, name string) ([]*api.ObjectEvent, error) {
	q := (&api.QueryOptions{Namespace: namespace}).WithContext(ctx)
	events, _, err := b.client.Objects().Events(kind.Group, kind.Kind, name, q)
	if err != nil {
		return nil, objectError(kind, namespace, name, err)
	}
	return events, nil
}

func (b *apiBackend) Stream(ctx context.Context, namespace string, topics map[api.Topic][]string, index uint64, exact bool) (<-chan *api.Events, error) {
	q := &api.QueryOptions{Namespace: namespace}
	if exact {
		return b.client.EventStream().StreamExact(ctx, topics, index, q)
	}
	return b.client.EventStream().Stream(ctx, topics, index, q)
}

func (b *apiBackend) ListJobs(ctx context.Context, namespace string, waitIndex uint64) ([]*api.JobListStub, uint64, error) {
	q := (&api.QueryOptions{Namespace: namespace, WaitIndex: waitIndex}).WithContext(ctx)
	jobs, qm, err := b.client.Jobs().ListOptions(&api.JobListOptions{Fields: &api.JobListFields{Meta: true}}, q)
	if err != nil {
		return nil, 0, err
	}
	return jobs, qm.LastIndex, nil
}

func (b *apiBackend) ReadJob(ctx context.Context, namespace, id string) (*api.Job, error) {
	q := (&api.QueryOptions{Namespace: namespace}).WithContext(ctx)
	job, _, err := b.client.Jobs().Info(id, q)
	if err != nil {
		if statusCode(err) == http.StatusNotFound {
			return nil, fmt.Errorf("job %s/%s: %w", namespace, id, ErrNotFound)
		}
		return nil, err
	}
	return job, nil
}

func (b *apiBackend) RegisterJob(ctx context.Context, job *api.Job) error {
	ns := ""
	if job.Namespace != nil {
		ns = *job.Namespace
	}
	w := (&api.WriteOptions{Namespace: ns}).WithContext(ctx)
	_, _, err := b.client.Jobs().Register(job, w)
	return err
}

func (b *apiBackend) DeregisterJob(ctx context.Context, namespace, id string, purge bool) error {
	w := (&api.WriteOptions{Namespace: namespace}).WithContext(ctx)
	_, _, err := b.client.Jobs().Deregister(id, purge, w)
	if statusCode(err) == http.StatusNotFound {
		return nil
	}
	return err
}

// NewAPIClient returns an API client configured from the environment like
// api.DefaultConfig (NORCA_ADDR, NORCA_TOKEN, ... with the NOMAD_ names as
// fallback), with one addition: the address may be a unix socket
// ("unix:///path/to/api.sock"), as used to reach the task API socket
// (secrets/api.sock) of a task with its workload identity.
func NewAPIClient() (*api.Client, error) {
	conf := api.DefaultConfig()
	if err := configureUnixSocket(conf, conf.Address); err != nil {
		return nil, err
	}
	return api.NewClient(conf)
}

func configureUnixSocket(conf *api.Config, addr string) error {
	socket, ok := strings.CutPrefix(addr, "unix://")
	if !ok {
		return nil
	}
	// Not parsed as a URL: on Windows the path is a drive path
	// (unix://C:\...\secrets\api.sock).
	if socket == "" {
		return errors.New("the unix socket address has no path")
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	conf.Address = "http://127.0.0.1"
	conf.HttpClient = &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", socket)
			},
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     90 * time.Second,
		},
	}
	return nil
}
