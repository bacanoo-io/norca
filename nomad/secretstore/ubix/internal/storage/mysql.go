package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql" // MySQL/MariaDB driver (D-010); importing it registers "mysql"
)

// MySQL/MariaDB backend schema. Values are opaque barrier ciphertext, so the
// database never sees plaintext (docs/design/sql-storage-backend.md, ADR D-014).
const (
	mysqlKVTable       = "ubixvault_kv"
	mysqlSchemaTable   = "ubixvault_schema"
	mysqlSchemaVersion = 2 // len(mysqlMigrations); see there for what each adds
	// mysqlMaxKeyLen bounds a key to the VARBINARY(768) primary key. Keys are
	// paths, so this is generous; longer keys are rejected rather than truncated.
	mysqlMaxKeyLen = 768
)

// MySQLBackend is a [Backend] over a MySQL/MariaDB database. It stores each entry
// as one row in a VARBINARY-keyed table — VARBINARY (not VARCHAR) so keys compare
// byte-for-byte, matching the file and in-memory backends rather than MySQL's
// case-insensitive default collation.
//
// Exactly one active vault process may write to a given database at a time.
// Replicas sharing a database elect that writer with [MySQLBackend.HALock], and
// once a replica has held the lock its writes are fenced to it (ADR D-021).
type MySQLBackend struct {
	db *sql.DB

	fenceMu sync.RWMutex
	fence   *fenceToken // nil until this replica first acquires an HA lock
}

// NewMySQLBackend opens a connection pool to the server described by dsn (a
// go-sql-driver DSN), verifies connectivity, and ensures the schema exists.
func NewMySQLBackend(dsn string) (*MySQLBackend, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: open mysql: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(3 * time.Minute)

	b := &MySQLBackend{db: db}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: connect mysql: %w", err)
	}
	// Not ctx: a migration may legitimately outlast a connection check.
	if err := b.ensureSchema(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return b, nil
}

// mysqlMigrations[i] takes the schema from version i to i+1. Every statement
// must be idempotent (IF NOT EXISTS and the like): MySQL commits DDL
// implicitly, so a process that dies partway through a migration leaves it
// half-applied and unrecorded, and the next start runs it again from the top.
// Append new versions here; never edit or reorder a released one.
var mysqlMigrations = [][]string{
	// 1: the key/value table and the schema-version table.
	{
		"CREATE TABLE IF NOT EXISTS " + mysqlKVTable + " (" +
			"vault_key VARBINARY(768) NOT NULL, " +
			"value LONGBLOB NOT NULL, " +
			"PRIMARY KEY (vault_key)) ENGINE=InnoDB ROW_FORMAT=DYNAMIC",
	},
	// 2: the HA lock table (ADR D-021).
	{
		"CREATE TABLE IF NOT EXISTS " + mysqlLockTable + " (" +
			"name VARBINARY(64) NOT NULL, " +
			"holder_id VARBINARY(255) NOT NULL, " +
			"advertise VARBINARY(1024) NOT NULL, " +
			"generation BIGINT UNSIGNED NOT NULL, " +
			"expires_at DATETIME(6) NOT NULL, " +
			"PRIMARY KEY (name)) ENGINE=InnoDB",
	},
}

// mysqlSchemaLock is the MySQL named lock (GET_LOCK) that serializes
// migrations, so replicas starting together cannot run them concurrently.
const mysqlSchemaLock = "ubixvault_schema_migrate"

// Schema-step time limits. Reading the version is quick; a migration is DDL,
// which can take minutes on a large table, and a replica that finds another
// one migrating waits for it rather than failing its own start.
const (
	mysqlSchemaReadTimeout = 10 * time.Second
	mysqlSchemaLockWaitSec = 600 // GET_LOCK timeout, in seconds
	mysqlMigrateTimeout    = 30 * time.Minute
)

// mysqlErrNoSuchTable is ER_NO_SUCH_TABLE: a database uBixVault never started
// against has no schema table yet.
const mysqlErrNoSuchTable = 1146

// ErrSchemaTooNew is returned when the database was migrated by a newer uBix
// Vault than this one: its schema may hold data this binary would misread or
// clobber, so it refuses to start rather than guess.
var ErrSchemaTooNew = errors.New("storage: database schema is newer than this uBixVault supports")

// ensureSchema brings the database to [mysqlSchemaVersion], applying each
// missing migration in order and recording it, and refuses a database that is
// already past it.
//
// Every start reads the version first, without the migration lock and without
// DDL: a database that is already current — every ordinary restart — needs
// neither, so replicas restarting together do not queue behind one another.
// Only when a migration is due does a replica take the lock, read the version
// again under it (another replica may have just migrated), and migrate.
func (b *MySQLBackend) ensureSchema(ctx context.Context) error {
	readCtx, cancel := context.WithTimeout(ctx, mysqlSchemaReadTimeout)
	current, err := schemaVersion(readCtx, b.db)
	cancel()
	if err != nil {
		return err
	}
	if err := checkNotTooNew(current); err != nil || current == mysqlSchemaVersion {
		return err
	}

	ctx, cancel = context.WithTimeout(ctx, mysqlMigrateTimeout)
	defer cancel()
	// GET_LOCK is held by a connection, so take one for the duration.
	conn, err := b.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("storage: schema: %w", err)
	}
	defer func() { _ = conn.Close() }()

	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)", mysqlSchemaLock, mysqlSchemaLockWaitSec).Scan(&got); err != nil {
		return fmt.Errorf("storage: schema lock: %w", err)
	}
	if !got.Valid || got.Int64 != 1 {
		return fmt.Errorf("storage: schema lock: timed out waiting for another replica's migration")
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), "SELECT RELEASE_LOCK(?)", mysqlSchemaLock)
	}()

	if _, err := conn.ExecContext(ctx,
		"CREATE TABLE IF NOT EXISTS "+mysqlSchemaTable+" (version INT NOT NULL, PRIMARY KEY (version))"); err != nil {
		return fmt.Errorf("storage: schema: %w", err)
	}
	if current, err = schemaVersion(ctx, conn); err != nil {
		return err
	}
	if err := checkNotTooNew(current); err != nil {
		return err
	}
	for v := current + 1; v <= mysqlSchemaVersion; v++ {
		for _, stmt := range mysqlMigrations[v-1] {
			if _, err := conn.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("storage: migrate schema to version %d: %w", v, err)
			}
		}
		if _, err := conn.ExecContext(ctx,
			"INSERT INTO "+mysqlSchemaTable+" (version) VALUES (?)", v); err != nil {
			return fmt.Errorf("storage: record schema version %d: %w", v, err)
		}
	}
	return nil
}

// schemaVersion reads the recorded schema version; 0 when the schema table does
// not exist yet.
func schemaVersion(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (int, error) {
	var v int
	err := q.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM "+mysqlSchemaTable).Scan(&v)
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) && myErr.Number == mysqlErrNoSuchTable {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("storage: read schema version: %w", err)
	}
	return v, nil
}

func checkNotTooNew(current int) error {
	if current > mysqlSchemaVersion {
		return fmt.Errorf("%w: database is at version %d, this binary knows up to %d — "+
			"upgrade uBixVault, or restore a snapshot taken before the newer version first ran",
			ErrSchemaTooNew, current, mysqlSchemaVersion)
	}
	return nil
}

// Close releases the connection pool.
func (b *MySQLBackend) Close() error { return b.db.Close() }

func (b *MySQLBackend) checkKey(key string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	if len(key) > mysqlMaxKeyLen {
		return fmt.Errorf("%w: key exceeds %d bytes", ErrInvalidKey, mysqlMaxKeyLen)
	}
	return nil
}

// Get returns the entry at key, or (nil, nil) if it does not exist.
func (b *MySQLBackend) Get(ctx context.Context, key string) (*Entry, error) {
	if err := b.checkKey(key); err != nil {
		return nil, err
	}
	var value []byte
	err := b.db.QueryRowContext(ctx,
		"SELECT value FROM "+mysqlKVTable+" WHERE vault_key = ?", []byte(key)).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("storage: mysql get: %w", err)
	}
	return &Entry{Key: key, Value: value}, nil
}

// Put stores entry, overwriting any existing value at entry.Key.
func (b *MySQLBackend) Put(ctx context.Context, entry *Entry) error {
	if err := b.checkKey(entry.Key); err != nil {
		return err
	}
	err := b.exec(ctx,
		"INSERT INTO "+mysqlKVTable+" (vault_key, value) VALUES (?, ?) "+
			"ON DUPLICATE KEY UPDATE value = VALUES(value)", []byte(entry.Key), entry.Value)
	if err != nil {
		return fmt.Errorf("storage: mysql put: %w", err)
	}
	return nil
}

// Delete removes the value at key. It is a no-op if key does not exist.
func (b *MySQLBackend) Delete(ctx context.Context, key string) error {
	if err := b.checkKey(key); err != nil {
		return err
	}
	if err := b.exec(ctx,
		"DELETE FROM "+mysqlKVTable+" WHERE vault_key = ?", []byte(key)); err != nil {
		return fmt.Errorf("storage: mysql delete: %w", err)
	}
	return nil
}

// List returns the immediate children under prefix. It range-scans the primary
// key for rows sharing the prefix, then reduces them to immediate children with
// the same logic the file and in-memory backends use, so the semantics are
// identical.
func (b *MySQLBackend) List(ctx context.Context, prefix string) ([]string, error) {
	if err := validatePrefix(prefix); err != nil {
		return nil, err
	}

	var rows *sql.Rows
	var err error
	if prefix == "" {
		rows, err = b.db.QueryContext(ctx, "SELECT vault_key FROM "+mysqlKVTable)
	} else if hi := prefixSuccessor([]byte(prefix)); hi != nil {
		rows, err = b.db.QueryContext(ctx,
			"SELECT vault_key FROM "+mysqlKVTable+" WHERE vault_key >= ? AND vault_key < ?", []byte(prefix), hi)
	} else {
		rows, err = b.db.QueryContext(ctx,
			"SELECT vault_key FROM "+mysqlKVTable+" WHERE vault_key >= ?", []byte(prefix))
	}
	if err != nil {
		return nil, fmt.Errorf("storage: mysql list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	seen := make(map[string]struct{})
	children := make([]string, 0) // non-nil: an empty result must DeepEqual []string{}, not nil
	for rows.Next() {
		var kb []byte
		if err := rows.Scan(&kb); err != nil {
			return nil, fmt.Errorf("storage: mysql list scan: %w", err)
		}
		child, ok := childUnder(prefix, string(kb))
		if !ok {
			continue
		}
		if _, dup := seen[child]; dup {
			continue
		}
		seen[child] = struct{}{}
		children = append(children, child)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: mysql list rows: %w", err)
	}
	sort.Strings(children)
	return children, nil
}

// prefixSuccessor returns the smallest byte string greater than every string
// with prefix p — p with its last non-0xFF byte incremented and the rest
// truncated. It returns nil when p is all 0xFF bytes (no finite upper bound).
func prefixSuccessor(p []byte) []byte {
	s := append([]byte(nil), p...)
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] != 0xFF {
			s[i]++
			return s[:i+1]
		}
	}
	return nil
}
