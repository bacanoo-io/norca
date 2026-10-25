//go:build integration

// These tests need a real MySQL/MariaDB and are gated behind the "integration"
// build tag; they skip unless UBIXVAULT_MARIADB_DSN is set. Run locally with:
//
//	docker run --rm -e MARIADB_ROOT_PASSWORD=root -e MARIADB_DATABASE=testdb -p 3306:3306 mariadb:11
//	UBIXVAULT_MARIADB_DSN='root:root@tcp(127.0.0.1:3306)/testdb' \
//	  go test -tags integration ./internal/storage/
package storage

import (
	"context"
	"os"
	"testing"
)

func mysqlDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("UBIXVAULT_MARIADB_DSN")
	if dsn == "" {
		t.Skip("set UBIXVAULT_MARIADB_DSN to run MySQL storage backend tests")
	}
	return dsn
}

// TestMySQLBackendConformance runs the shared Backend contract suite against a
// real database — the same suite the file and in-memory backends pass, proving
// the SQL backend is interchangeable with them.
func TestMySQLBackendConformance(t *testing.T) {
	dsn := mysqlDSN(t)
	runBackendConformance(t, func(t *testing.T) Backend {
		b, err := NewMySQLBackend(dsn)
		if err != nil {
			t.Fatalf("NewMySQLBackend: %v", err)
		}
		// Each conformance subtest expects an empty backend.
		if _, err := b.db.ExecContext(context.Background(), "DELETE FROM "+mysqlKVTable); err != nil {
			t.Fatalf("clear table: %v", err)
		}
		t.Cleanup(func() { _ = b.Close() })
		return b
	})
}

func TestMySQLBackendRejectsOverlongKey(t *testing.T) {
	dsn := mysqlDSN(t)
	b, err := NewMySQLBackend(dsn)
	if err != nil {
		t.Fatalf("NewMySQLBackend: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })

	long := make([]byte, mysqlMaxKeyLen+1)
	for i := range long {
		long[i] = 'a'
	}
	if err := b.Put(context.Background(), &Entry{Key: string(long), Value: []byte("x")}); err == nil {
		t.Fatal("Put with an overlong key should fail")
	}
}

func TestMySQLBackendBinaryValueAndUpsert(t *testing.T) {
	dsn := mysqlDSN(t)
	ctx := context.Background()
	b, err := NewMySQLBackend(dsn)
	if err != nil {
		t.Fatalf("NewMySQLBackend: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	if _, err := b.db.ExecContext(ctx, "DELETE FROM "+mysqlKVTable); err != nil {
		t.Fatalf("clear: %v", err)
	}

	// Binary value with NULs and high bytes round-trips intact.
	val := []byte{0x00, 0xFF, 0x10, 0x00, 0x7F, 0x80}
	if err := b.Put(ctx, &Entry{Key: "bin/blob", Value: val}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := b.Get(ctx, "bin/blob")
	if err != nil || got == nil || string(got.Value) != string(val) {
		t.Fatalf("binary round-trip = %+v, %v", got, err)
	}

	// Overwriting the same key replaces the value (upsert).
	if err := b.Put(ctx, &Entry{Key: "bin/blob", Value: []byte("replaced")}); err != nil {
		t.Fatalf("Put overwrite: %v", err)
	}
	got, _ = b.Get(ctx, "bin/blob")
	if got == nil || string(got.Value) != "replaced" {
		t.Fatalf("after upsert = %+v, want replaced", got)
	}
}
