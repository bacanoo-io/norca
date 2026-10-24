// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package natsbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/controller"
)

// Checkpointer persists the index of the last event batch published.
type Checkpointer interface {
	// Load returns the stored index; found is false if there is none yet.
	Load(ctx context.Context) (index uint64, found bool, err error)

	// Save stores index. Implementations never move a stored checkpoint
	// backwards.
	Save(ctx context.Context, index uint64) error
}

// Items of a checkpoint Variable.
const (
	checkpointItemIndex   = "index"
	checkpointItemRegion  = "region"
	checkpointItemUpdated = "updated"
	checkpointItemWriter  = "writer"
)

// VariableCheckpoint keeps the checkpoint in a Variable. It is the default:
// every instance of the bridge job can read it, so an instance that takes
// over the leadership on another node continues where the previous leader
// stopped.
//
// Writes use check-and-set and never lower the stored index, so a former
// leader that has not noticed the loss of its lease yet cannot move the
// checkpoint of the new leader backwards.
type VariableCheckpoint struct {
	Vars      controller.VariablesAPI
	Namespace string
	Path      string
	Region    string
	Writer    string

	modifyIndex uint64
}

func (c *VariableCheckpoint) Load(ctx context.Context) (uint64, bool, error) {
	v, err := c.Vars.ReadVariable(ctx, c.Namespace, c.Path)
	if errors.Is(err, controller.ErrNotFound) {
		c.modifyIndex = 0
		return 0, false, nil
	} else if err != nil {
		return 0, false, fmt.Errorf("reading the checkpoint %s/%s: %w", c.Namespace, c.Path, err)
	}
	c.modifyIndex = v.ModifyIndex
	index, err := strconv.ParseUint(v.Items[checkpointItemIndex], 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("invalid index in the checkpoint %s/%s: %w", c.Namespace, c.Path, err)
	}
	return index, true, nil
}

func (c *VariableCheckpoint) Save(ctx context.Context, index uint64) error {
	for attempt := 0; ; attempt++ {
		out, err := c.Vars.WriteVariable(ctx, &api.Variable{
			Namespace:   c.Namespace,
			Path:        c.Path,
			ModifyIndex: c.modifyIndex,
			Items: api.VariableItems{
				checkpointItemIndex:   strconv.FormatUint(index, 10),
				checkpointItemRegion:  c.Region,
				checkpointItemUpdated: time.Now().UTC().Format(time.RFC3339Nano),
				checkpointItemWriter:  c.Writer,
			},
		})
		if err == nil {
			c.modifyIndex = out.ModifyIndex
			return nil
		}
		if !errors.Is(err, controller.ErrConflict) || attempt == 3 {
			return fmt.Errorf("writing the checkpoint %s/%s: %w", c.Namespace, c.Path, err)
		}
		stored, found, err := c.Load(ctx)
		if err != nil {
			return err
		}
		if found && stored >= index {
			return nil
		}
	}
}

// FileCheckpoint keeps the checkpoint in a local file. It suits a single
// instance outside of the cluster, or a task with a host volume; it is not
// shared between instances.
type FileCheckpoint struct {
	Path   string
	Region string
}

type fileCheckpoint struct {
	Index   uint64
	Region  string
	Updated time.Time
}

func (c *FileCheckpoint) Load(context.Context) (uint64, bool, error) {
	b, err := os.ReadFile(c.Path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	} else if err != nil {
		return 0, false, err
	}
	var cp fileCheckpoint
	if err := json.Unmarshal(b, &cp); err != nil {
		return 0, false, fmt.Errorf("invalid checkpoint file %s: %w", c.Path, err)
	}
	return cp.Index, true, nil
}

func (c *FileCheckpoint) Save(ctx context.Context, index uint64) error {
	if stored, found, err := c.Load(ctx); err == nil && found && stored >= index {
		return nil
	}
	b, err := json.Marshal(fileCheckpoint{Index: index, Region: c.Region, Updated: time.Now().UTC()})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.Path), filepath.Base(c.Path)+".tmp*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(append(b, '\n'))
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), c.Path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}

// MemoryCheckpoint keeps the checkpoint in memory: a restarted process
// starts again without a checkpoint. It is used with -checkpoint=none and
// in tests.
type MemoryCheckpoint struct {
	mu    sync.Mutex
	index uint64
	found bool
}

func (c *MemoryCheckpoint) Load(context.Context) (uint64, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.index, c.found, nil
}

func (c *MemoryCheckpoint) Save(_ context.Context, index uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.found || index > c.index {
		c.index, c.found = index, true
	}
	return nil
}
