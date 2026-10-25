//go:build integration

package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

var schemaDBSeq atomic.Int64

// freshDatabase creates an empty database for one test and returns its DSN and
// an admin handle on it; both are cleaned up with the test.
func freshDatabase(t *testing.T) (string, *sql.DB) {
	t.Helper()
	cfg, err := mysql.ParseDSN(mysqlDSN(t))
	if err != nil {
		t.Fatalf("ParseDSN: %v", err)
	}
	name := fmt.Sprintf("uv_schema_%d_%d", schemaDBSeq.Add(1), len(t.Name()))
	root, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	ctx := context.Background()
	if _, err := root.ExecContext(ctx, "DROP DATABASE IF EXISTS "+name); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := root.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _, _ = root.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name) })

	cfg.DBName = name
	dsn := cfg.FormatDSN()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return dsn, db
}

func schemaVersions(t *testing.T, db *sql.DB) []int {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), "SELECT version FROM "+mysqlSchemaTable+" ORDER BY version")
	if err != nil {
		t.Fatalf("read versions: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, v)
	}
	return out
}

func wantVersions(t *testing.T, db *sql.DB) {
	t.Helper()
	got := schemaVersions(t, db)
	if len(got) != mysqlSchemaVersion {
		t.Fatalf("versions = %v, want 1..%d each exactly once", got, mysqlSchemaVersion)
	}
	for i, v := range got {
		if v != i+1 {
			t.Fatalf("versions = %v, want 1..%d each exactly once", got, mysqlSchemaVersion)
		}
	}
}

func TestSchemaFreshDatabase(t *testing.T) {
	dsn, db := freshDatabase(t)
	b, err := NewMySQLBackend(dsn)
	if err != nil {
		t.Fatalf("NewMySQLBackend: %v", err)
	}
	_ = b.Close()
	wantVersions(t, db)

	// Starting again is a no-op.
	b, err = NewMySQLBackend(dsn)
	if err != nil {
		t.Fatalf("second start: %v", err)
	}
	_ = b.Close()
	wantVersions(t, db)
}

// TestSchemaUpgradesVersion1 starts against a database exactly as uBixVault
// 1.1 left it: data intact, version 2 applied and recorded.
func TestSchemaUpgradesVersion1(t *testing.T) {
	ctx := context.Background()
	dsn, db := freshDatabase(t)
	for _, stmt := range []string{
		mysqlMigrations[0][0],
		"CREATE TABLE " + mysqlSchemaTable + " (version INT NOT NULL, PRIMARY KEY (version))",
		"INSERT INTO " + mysqlSchemaTable + " (version) VALUES (1)",
		"INSERT INTO " + mysqlKVTable + " (vault_key, value) VALUES ('core/keyring', 'ciphertext')",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed v1: %v", err)
		}
	}

	b, err := NewMySQLBackend(dsn)
	if err != nil {
		t.Fatalf("NewMySQLBackend on v1: %v", err)
	}
	defer func() { _ = b.Close() }()
	wantVersions(t, db)
	got, err := b.Get(ctx, "core/keyring")
	if err != nil || got == nil || string(got.Value) != "ciphertext" {
		t.Fatalf("v1 data after upgrade = %+v, %v", got, err)
	}
	if _, err := b.HALock("active", "a", "", LockOptions{}).Holder(ctx); err != nil {
		t.Fatalf("lock table missing after upgrade: %v", err)
	}
}

func TestSchemaRefusesNewerDatabase(t *testing.T) {
	ctx := context.Background()
	dsn, db := freshDatabase(t)
	b, err := NewMySQLBackend(dsn)
	if err != nil {
		t.Fatalf("NewMySQLBackend: %v", err)
	}
	_ = b.Close()
	// A future release migrated this database further.
	if _, err := db.ExecContext(ctx, "INSERT INTO "+mysqlSchemaTable+" (version) VALUES (?)", mysqlSchemaVersion+1); err != nil {
		t.Fatalf("simulate newer schema: %v", err)
	}

	_, err = NewMySQLBackend(dsn)
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("start against newer schema = %v, want ErrSchemaTooNew", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("version %d", mysqlSchemaVersion+1)) {
		t.Fatalf("error does not name the database's version: %v", err)
	}
}

// TestSchemaConcurrentStartsMigrateOnce: replicas starting at the same moment
// against a fresh database all come up, and each version is recorded once.
func TestSchemaConcurrentStartsMigrateOnce(t *testing.T) {
	dsn, db := freshDatabase(t)
	const replicas = 6
	var wg sync.WaitGroup
	errs := make(chan error, replicas)
	for range replicas {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, err := NewMySQLBackend(dsn)
			if err == nil {
				_ = b.Close()
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent start: %v", err)
		}
	}
	wantVersions(t, db)
}

// holdSchemaLock takes the migration lock on its own connection, as a replica
// in the middle of a migration would, and returns a func releasing it.
func holdSchemaLock(t *testing.T, db *sql.DB) func() {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	var got int
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 5)", mysqlSchemaLock).Scan(&got); err != nil || got != 1 {
		t.Fatalf("GET_LOCK = %d, %v", got, err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			_, _ = conn.ExecContext(ctx, "SELECT RELEASE_LOCK(?)", mysqlSchemaLock)
			_ = conn.Close()
		})
	}
	t.Cleanup(release)
	return release
}

// TestSchemaCurrentStartSkipsLock: an ordinary restart against a current
// schema needs neither the migration lock nor DDL, so it is not held up by
// another replica holding the lock.
func TestSchemaCurrentStartSkipsLock(t *testing.T) {
	dsn, db := freshDatabase(t)
	b, err := NewMySQLBackend(dsn)
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	_ = b.Close()

	holdSchemaLock(t, db)
	start := time.Now()
	b, err = NewMySQLBackend(dsn)
	if err != nil {
		t.Fatalf("restart while another replica holds the migration lock: %v", err)
	}
	_ = b.Close()
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("restart against a current schema took %v; it should not wait for the lock", took)
	}
}

// TestSchemaMigrationWaitsForLockHolder: a replica that needs to migrate while
// another holds the lock waits for it, then finds the work done or does it.
func TestSchemaMigrationWaitsForLockHolder(t *testing.T) {
	dsn, db := freshDatabase(t)
	release := holdSchemaLock(t, db)

	done := make(chan error, 1)
	go func() {
		b, err := NewMySQLBackend(dsn)
		if err == nil {
			_ = b.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("migrated while another replica held the lock (err %v)", err)
	case <-time.After(500 * time.Millisecond):
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("start after the lock was released: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("start did not proceed after the lock was released")
	}
	wantVersions(t, db)
}
