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

	"github.com/bacanoo-io/norca/norca/helper/raftbadger"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/hashicorp/go-msgpack/codec"
	"github.com/hashicorp/raft"
)

var (
	errAlreadyOpen = errors.New("unable to open raft logs that are in use")
)

// RaftStateInfo opens the raft store (the Badger directory p) read-only and
// returns it with its first and last index.
func RaftStateInfo(p string) (store *raftbadger.Store, firstIdx uint64, lastIdx uint64, err error) {
	s, err := raftbadger.Open(raftbadger.Options{Path: p, ReadOnly: true})
	if err != nil {
		if strings.Contains(err.Error(), "Cannot acquire directory lock") || strings.Contains(err.Error(), "Cannot create lock file") {
			return nil, 0, 0, errAlreadyOpen
		}
		return nil, 0, 0, fmt.Errorf("failed to open raft logs: %v", err)
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

// raftStoreDir is the directory of the raft store inside the raft directory.
const raftStoreDir = "badger"

// FindRaftFile finds the raft store (the Badger directory) in a data
// directory, a raft directory or the store directory itself, and returns its
// path. A BoltDB raft.db of an older Norca is reported, not read.
func FindRaftFile(p string) (raftpath string, err error) {
	for _, c := range []string{
		filepath.Join(p, "server", "raft", raftStoreDir),
		filepath.Join(p, "raft", raftStoreDir),
		filepath.Join(p, raftStoreDir),
		p,
	} {
		if raftbadger.IsStore(c) {
			return c, nil
		}
	}
	for _, c := range []string{filepath.Join(p, "server", "raft", "raft.db"), filepath.Join(p, "raft", "raft.db"), filepath.Join(p, "raft.db"), p} {
		if st, err := os.Stat(c); err == nil && st.Mode().IsRegular() && filepath.Base(c) == "raft.db" {
			return "", fmt.Errorf("%s is a BoltDB raft store of an older Norca; this version keeps raft in Badger (raft/%s)", c, raftStoreDir)
		}
	}
	manifest, err := FindFileInPath("MANIFEST", p)
	if err != nil {
		return "", fmt.Errorf("no raft store found in %s", p)
	}
	return filepath.Dir(manifest), nil
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
