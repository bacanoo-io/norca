// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package dynamic hands out anonymous user IDs to tasks that do not name a
// user, for drivers that run tasks without an image (exec2).
//
// A task gets a UID from a range reserved on the client for this purpose and
// the same value as GID. The user does not exist in /etc/passwd: the task
// runner passes it to the driver as a synthetic user name, which the driver
// turns back into the IDs with LookupUser.
package dynamic

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/bacanoo-io/norca/norca/helper/users"
)

// UGID is a dynamic user ID, used as both UID and GID.
type UGID int

const (
	// DefaultMin and DefaultMax bound the range of dynamic user IDs when the
	// client configuration does not set one.
	DefaultMin UGID = 80_000
	DefaultMax UGID = 89_999

	// Home is the home directory of a dynamic user: there is none.
	Home = users.DynamicHome
)

var (
	// ErrPoolExhausted is returned when every ID of the range is in use.
	ErrPoolExhausted = errors.New("no free dynamic user ID")

	// ErrNotDynamic is returned by Parse for the name of a real user.
	ErrNotDynamic = errors.New("not a dynamic user")
)

// String is the synthetic user name of id, as the driver receives it.
func String(id UGID) string {
	return users.DynamicPrefix + strconv.Itoa(int(id))
}

// Parse returns the ID in a synthetic user name, or ErrNotDynamic when user
// is not one.
func Parse(user string) (UGID, error) {
	rest, ok := strings.CutPrefix(user, users.DynamicPrefix)
	if !ok {
		return 0, ErrNotDynamic
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid dynamic user %q", user)
	}
	return UGID(n), nil
}

// LookupUser returns the UID, GID and home directory of user, which is
// either a synthetic name made by String or the name of a real user.
func LookupUser(user string) (int, int, string, error) {
	if _, err := Parse(user); err != nil && !errors.Is(err, ErrNotDynamic) {
		return 0, 0, "", err
	}

	// users.Lookup answers a dynamic user from its name
	u, err := users.Lookup(user)
	if err != nil {
		return 0, 0, "", fmt.Errorf("failed to look up user %q: %w", user, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, "", fmt.Errorf("user %q has a non-numeric uid %q", user, u.Uid)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return 0, 0, "", fmt.Errorf("user %q has a non-numeric gid %q", user, u.Gid)
	}
	return uid, gid, u.HomeDir, nil
}

// Pool hands out the IDs of a range, each to one task at a time.
type Pool interface {
	// Acquire reserves a free ID.
	Acquire() (UGID, error)

	// Restore marks id as in use again, for a task the client recovers
	// after a restart. An ID outside the range is ignored.
	Restore(id UGID)

	// Release frees id. Releasing a free ID does nothing.
	Release(id UGID)
}

// PoolConfig is the range of a Pool, inclusive at both ends.
type PoolConfig struct {
	Min UGID
	Max UGID
}

// Validate checks that the range is usable.
func (c PoolConfig) Validate() error {
	switch {
	case c.Min <= 0 || c.Max <= 0:
		return fmt.Errorf("dynamic user range must be positive, got %d-%d", c.Min, c.Max)
	case c.Min > c.Max:
		return fmt.Errorf("dynamic user range start %d is above its end %d", c.Min, c.Max)
	}
	return nil
}

// New returns a Pool over the range of c, which must be valid.
func New(c PoolConfig) Pool {
	return &pool{min: c.Min, max: c.Max, next: c.Min, used: make(map[UGID]struct{})}
}

type pool struct {
	lock     sync.Mutex
	min, max UGID
	next     UGID // where the search for a free ID starts
	used     map[UGID]struct{}
}

func (p *pool) Acquire() (UGID, error) {
	p.lock.Lock()
	defer p.lock.Unlock()

	size := int(p.max-p.min) + 1
	if len(p.used) >= size {
		return 0, ErrPoolExhausted
	}

	// round robin from the last handed out ID, so that an ID just released
	// is not handed out again right away
	id := p.next
	for range size {
		if _, taken := p.used[id]; !taken {
			p.used[id] = struct{}{}
			p.next = p.wrap(id + 1)
			return id, nil
		}
		id = p.wrap(id + 1)
	}
	return 0, ErrPoolExhausted
}

func (p *pool) wrap(id UGID) UGID {
	if id > p.max {
		return p.min
	}
	return id
}

func (p *pool) Restore(id UGID) {
	if id < p.min || id > p.max {
		return
	}
	p.lock.Lock()
	defer p.lock.Unlock()
	p.used[id] = struct{}{}
}

func (p *pool) Release(id UGID) {
	p.lock.Lock()
	defer p.lock.Unlock()
	delete(p.used, id)
}
