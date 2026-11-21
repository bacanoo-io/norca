// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package consulkv

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/shoenig/test/must"
)

// failingStatus is a bucket whose status cannot be read.
type failingStatus struct {
	jetstream.KeyValue
}

func (failingStatus) Status(context.Context) (jetstream.KeyValueStatus, error) {
	return nil, errors.New("no status")
}

// A failure to read the status of the bucket fails the query: taken for 0, it
// gave the index 1 to a caller that had a higher one (review finding HLP-9).
func TestGet_StatusFailureFailsTheQuery(t *testing.T) {
	kv := testBucket(t)
	_, err := kv.Put(context.Background(), "s.other", []byte("x"))
	must.NoError(t, err)

	_, index, err := Get(context.Background(), kv, "s.", "missing", false, 0, time.Second)
	must.NoError(t, err)
	must.Greater(t, uint64(1), index)

	_, _, err = Get(context.Background(), failingStatus{kv}, "s.", "missing", false, 0, time.Second)
	must.ErrorContains(t, err, "no status")
}
