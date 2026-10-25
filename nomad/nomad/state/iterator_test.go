// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"testing"

	memdb "github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
)

// SliceIterator is returned from state store methods as a
// memdb.ResultIterator.
var _ memdb.ResultIterator = (*SliceIterator)(nil)

func TestSliceIterator_Empty(t *testing.T) {
	ci.Parallel(t)

	iter := NewSliceIterator()
	must.Nil(t, iter.Next())
	// exhausted iterators keep returning nil
	must.Nil(t, iter.Next())
	must.Nil(t, iter.WatchCh())
}

func TestSliceIterator_Order(t *testing.T) {
	ci.Parallel(t)

	iter := NewSliceIterator()
	inputs := []interface{}{"a", 2, "c", struct{ X int }{X: 4}}
	for _, in := range inputs {
		iter.Add(in)
	}

	var got []interface{}
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		got = append(got, raw)
	}
	must.Eq(t, inputs, got)
	must.Nil(t, iter.Next())
}

func TestSliceIterator_AddWhileIterating(t *testing.T) {
	ci.Parallel(t)

	iter := NewSliceIterator()
	iter.Add("first")
	iter.Add("second")

	must.Eq[interface{}](t, "first", iter.Next())

	// items added after iteration has started are still returned in order
	iter.Add("third")
	must.Eq[interface{}](t, "second", iter.Next())
	must.Eq[interface{}](t, "third", iter.Next())
	must.Nil(t, iter.Next())

	// and an exhausted iterator picks up newly added items
	iter.Add("fourth")
	must.Eq[interface{}](t, "fourth", iter.Next())
	must.Nil(t, iter.Next())
}

func TestSliceIterator_ReturnsSameValues(t *testing.T) {
	ci.Parallel(t)

	type item struct{ ID string }
	a, b := &item{ID: "a"}, &item{ID: "b"}

	iter := NewSliceIterator()
	iter.Add(a)
	iter.Add(b)

	// the iterator hands back the stored values themselves, not copies
	must.True(t, iter.Next().(*item) == a)
	must.True(t, iter.Next().(*item) == b)
	must.Nil(t, iter.Next())
}
