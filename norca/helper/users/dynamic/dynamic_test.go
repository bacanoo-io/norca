// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package dynamic

import (
	"errors"
	"sync"
	"testing"

	"github.com/bacanoo-io/norca/norca/helper/users"
	"github.com/shoenig/test/must"
)

func TestStringParse(t *testing.T) {
	name := String(80_042)
	must.Eq(t, "norca:80042", name)

	id, err := Parse(name)
	must.NoError(t, err)
	must.Eq(t, UGID(80_042), id)

	_, err = Parse("nobody")
	must.True(t, errors.Is(err, ErrNotDynamic))

	for _, bad := range []string{"norca:", "norca:x", "norca:-1", "norca:0"} {
		_, err = Parse(bad)
		must.Error(t, err, must.Sprint(bad))
		must.False(t, errors.Is(err, ErrNotDynamic), must.Sprint(bad))
	}
}

func TestLookupUser_dynamic(t *testing.T) {
	uid, gid, home, err := LookupUser("norca:80001")
	must.NoError(t, err)
	must.Eq(t, 80001, uid)
	must.Eq(t, 80001, gid)
	must.Eq(t, Home, home)

	_, _, _, err = LookupUser("norca:nope")
	must.Error(t, err)
}

func TestPoolConfig_Validate(t *testing.T) {
	must.NoError(t, PoolConfig{Min: DefaultMin, Max: DefaultMax}.Validate())
	must.NoError(t, PoolConfig{Min: 5, Max: 5}.Validate())
	must.Error(t, PoolConfig{Min: 0, Max: 5}.Validate())
	must.Error(t, PoolConfig{Min: 6, Max: 5}.Validate())
}

func TestPool_acquireRelease(t *testing.T) {
	p := New(PoolConfig{Min: 10, Max: 12})

	a, err := p.Acquire()
	must.NoError(t, err)
	b, err := p.Acquire()
	must.NoError(t, err)
	c, err := p.Acquire()
	must.NoError(t, err)
	must.Eq(t, []UGID{10, 11, 12}, []UGID{a, b, c})

	_, err = p.Acquire()
	must.True(t, errors.Is(err, ErrPoolExhausted))

	p.Release(b)
	p.Release(b) // releasing twice is harmless
	got, err := p.Acquire()
	must.NoError(t, err)
	must.Eq(t, b, got)
}

func TestPool_roundRobin(t *testing.T) {
	p := New(PoolConfig{Min: 10, Max: 12})
	a, _ := p.Acquire()
	p.Release(a)

	// the next ID comes after the last one handed out, not the freed one
	b, err := p.Acquire()
	must.NoError(t, err)
	must.Eq(t, UGID(11), b)
}

func TestPool_restore(t *testing.T) {
	p := New(PoolConfig{Min: 10, Max: 11})
	p.Restore(10)
	p.Restore(10)
	p.Restore(99) // outside the range: ignored

	got, err := p.Acquire()
	must.NoError(t, err)
	must.Eq(t, UGID(11), got)

	_, err = p.Acquire()
	must.True(t, errors.Is(err, ErrPoolExhausted))
}

func TestPool_concurrent(t *testing.T) {
	p := New(PoolConfig{Min: 1, Max: 200})
	var (
		wg   sync.WaitGroup
		lock sync.Mutex
		seen = make(map[UGID]bool)
	)
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := p.Acquire()
			must.NoError(t, err)
			lock.Lock()
			defer lock.Unlock()
			must.False(t, seen[id], must.Sprintf("id %d handed out twice", id))
			seen[id] = true
		}()
	}
	wg.Wait()
	must.MapLen(t, 200, seen)
}

func TestLookup_sharedWithUsersPackage(t *testing.T) {
	// helper/users answers dynamic names too, for the hooks that write
	// files owned by the task user
	u, err := users.Lookup(String(80_007))
	must.NoError(t, err)
	must.Eq(t, "80007", u.Uid)
	must.Eq(t, "80007", u.Gid)
	must.Eq(t, Home, u.HomeDir)

	uid, err := users.UIDforUser(String(80_007))
	must.NoError(t, err)
	must.Eq(t, 80_007, uid)
}
