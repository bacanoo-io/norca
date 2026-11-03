// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package raftutil

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bacanoo-io/norca/norca/helper/raftbadger"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/hashicorp/go-msgpack/codec"
	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	"go.etcd.io/bbolt"
)

// Store is a raft log store opened by the operator commands: Badger or
// BoltDB.
type Store interface {
	raft.LogStore
	raft.StableStore
	Close() error
}

var (
	errAlreadyOpen = errors.New("unable to open raft logs that are in use")
)

// RaftStateInfo opens the raft store p read-only (a Badger directory, or a
// BoltDB raft.db file) and returns it with its first and last index.
func RaftStateInfo(p string) (store Store, firstIdx uint64, lastIdx uint64, err error) {
	s, err := openStore(p)
	if err != nil {
		return nil, 0, 0, err
	}

	firstIdx, err = s.FirstIndex()
	if err != nil {
		return nil, 0, 0, fmt.Errorf("failed to fetch first index: %v", err)
	}

	lastIdx, err = s.LastIndex()
	if err != nil {
		return nil, 0, 0, fmt.Errorf("failed to fetch last index: %v", err)
	}

	return s, firstIdx, lastIdx, nil
}

// LogEntries reads the raft logs found in the data directory found at
// the path `p`, and returns a channel of logs, and a channel of
// warnings. If opening the raft state returns an error, both channels
// will be nil.
func LogEntries(p string) (<-chan interface{}, <-chan error, error) {
	store, firstIdx, lastIdx, err := RaftStateInfo(p)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open raft logs: %v", err)
	}

	entries := make(chan interface{})
	warnings := make(chan error)

	go func() {
		defer store.Close()
		defer close(entries)
		for i := firstIdx; i <= lastIdx; i++ {
			var e raft.Log
			err := store.GetLog(i, &e)
			if err != nil {
				warnings <- fmt.Errorf(
					"failed to read log entry at index %d (firstIdx: %d, lastIdx: %d): %v",
					i, firstIdx, lastIdx, err)
				continue
			}

			entry, err := decode(&e)
			if err != nil {
				warnings <- fmt.Errorf(
					"failed to decode log entry at index %d: %v", i, err)
				continue
			}

			entries <- entry
		}
	}()

	return entries, warnings, nil
}

type logMessage struct {
	LogType string
	Term    uint64
	Index   uint64

	CommandType           string      `json:",omitempty"`
	IgnoreUnknownTypeFlag bool        `json:",omitempty"`
	Body                  interface{} `json:",omitempty"`
}

func decode(e *raft.Log) (*logMessage, error) {
	m := &logMessage{
		LogType: logTypes[e.Type],
		Term:    e.Term,
		Index:   e.Index,
	}

	if m.LogType == "" {
		m.LogType = fmt.Sprintf("%d", e.Type)
	}

	var data []byte
	if e.Type == raft.LogCommand {
		if len(e.Data) == 0 {
			return nil, fmt.Errorf("command did not include data")
		}

		msgType := structs.MessageType(e.Data[0])

		m.CommandType = commandName(msgType & ^structs.IgnoreUnknownTypeFlag)
		m.IgnoreUnknownTypeFlag = (msgType & structs.IgnoreUnknownTypeFlag) != 0

		data = e.Data[1:]
	} else {
		data = e.Data
	}

	if len(data) != 0 {
		decoder := codec.NewDecoder(bytes.NewReader(data), structs.MsgpackHandle)

		var v interface{}
		var err error
		if m.CommandType == commandName(structs.JobBatchDeregisterRequestType) {
			var vr structs.JobBatchDeregisterRequest
			err = decoder.Decode(&vr)
			v = jsonifyJobBatchDeregisterRequest(&vr)
		} else {
			var vr interface{}
			err = decoder.Decode(&vr)
			v = vr
		}

		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to decode log entry at index %d: failed to decode body of %v.%v %v\n", e.Index, e.Type, m.CommandType, err)
			v = "FAILED TO DECODE DATA"
		}
		fixTime(v)
		m.Body = v
	}

	return m, nil
}

// jsonifyJobBatchDeregisterRequest special case JsonBatchDeregisterRequest object
// as the actual type is not json friendly.
func jsonifyJobBatchDeregisterRequest(v *structs.JobBatchDeregisterRequest) interface{} {
	var data struct {
		Jobs  map[string]*structs.JobDeregisterOptions
		Evals []*structs.Evaluation
		structs.WriteRequest
	}
	data.Evals = v.Evals
	data.WriteRequest = v.WriteRequest

	data.Jobs = make(map[string]*structs.JobDeregisterOptions, len(v.Jobs))
	if len(v.Jobs) != 0 {
		for k, v := range v.Jobs {
			data.Jobs[k.Namespace+"."+k.ID] = v
		}
	}
	return data
}

var logTypes = map[raft.LogType]string{
	raft.LogCommand:              "LogCommand",
	raft.LogNoop:                 "LogNoop",
	raft.LogAddPeerDeprecated:    "LogAddPeerDeprecated",
	raft.LogRemovePeerDeprecated: "LogRemovePeerDeprecated",
	raft.LogBarrier:              "LogBarrier",
	raft.LogConfiguration:        "LogConfiguration",
}

func commandName(mt structs.MessageType) string {
	n := msgTypeNames[mt]
	if n != "" {
		return n
	}

	return fmt.Sprintf("%v", mt)
}

// raftStoreDir and raftBoltFile are the raft stores inside the raft
// directory: Badger (the default) and BoltDB (raft_store = "boltdb").
const (
	raftStoreDir = "badger"
	raftBoltFile = "raft.db"
)

// isBoltFile reports whether p is a BoltDB raft.db file.
func isBoltFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular() && filepath.Ext(p) == ".db"
}

// openStore opens a raft store read-only.
func openStore(p string) (Store, error) {
	if isBoltFile(p) {
		s, err := raftboltdb.New(raftboltdb.Options{
			Path:        p,
			BoltOptions: &bbolt.Options{ReadOnly: true, Timeout: 1 * time.Second},
		})
		if err != nil {
			if strings.HasSuffix(err.Error(), "timeout") {
				return nil, errAlreadyOpen
			}
			return nil, fmt.Errorf("failed to open raft logs: %v", err)
		}
		return s, nil
	}
	s, err := raftbadger.Open(raftbadger.Options{Path: p, ReadOnly: true})
	if err != nil {
		if strings.Contains(err.Error(), "Cannot acquire directory lock") || strings.Contains(err.Error(), "Cannot create lock file") {
			return nil, errAlreadyOpen
		}
		return nil, fmt.Errorf("failed to open raft logs: %v", err)
	}
	return s, nil
}

// FindRaftFile finds the raft store in a data directory, a raft directory or
// the store itself and returns its path: the Badger directory, or the
// BoltDB raft.db file of a server with raft_store = "boltdb". A raft
// directory with both is ambiguous; then the store has to be named.
func FindRaftFile(p string) (raftpath string, err error) {
	if raftbadger.IsStore(p) || isBoltFile(p) {
		return p, nil
	}
	for _, dir := range []string{filepath.Join(p, "server", "raft"), filepath.Join(p, "raft"), p} {
		badger := filepath.Join(dir, raftStoreDir)
		bolt := filepath.Join(dir, raftBoltFile)
		hasBadger, hasBolt := raftbadger.IsStore(badger), isBoltFile(bolt)
		switch {
		case hasBadger && hasBolt:
			return "", fmt.Errorf("%s holds both a Badger (%s) and a BoltDB (%s) raft store; name the one to read",
				dir, raftStoreDir, raftBoltFile)
		case hasBadger:
			return badger, nil
		case hasBolt:
			return bolt, nil
		}
	}
	if manifest, err := FindFileInPath("MANIFEST", p); err == nil {
		return filepath.Dir(manifest), nil
	}
	if db, err := FindFileInPath(raftBoltFile, p); err == nil {
		return db, nil
	}
	return "", fmt.Errorf("no raft store found in %s", p)
}

// FindRaftDir finds the raft store and returns the raft directory that holds
// it (and the snapshots)
func FindRaftDir(p string) (raftpath string, err error) {
	raftpath, err = FindRaftFile(p)
	if err != nil {
		return "", err
	}

	if raftpath == "" {
		return "", fmt.Errorf("failed to find raft dir in %s", p)
	}

	return filepath.Dir(raftpath), nil
}

// FindFileInPath searches for file in path p
func FindFileInPath(file string, p string) (path string, err error) {
	// Define walk function to find file
	walkFn := func(walkPath string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if filepath.Base(walkPath) == file {
			path = walkPath
		}
		return nil
	}

	// Walk path p looking for file
	walkErr := filepath.Walk(p, walkFn)
	if walkErr != nil {
		return "", walkErr
	}

	if path == "" {
		return "", fmt.Errorf("File %s not found in path %s", file, p)
	}

	return path, nil
}
