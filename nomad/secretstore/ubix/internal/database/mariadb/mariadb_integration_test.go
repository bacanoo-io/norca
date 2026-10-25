//go:build integration

// These tests require a real MariaDB/MySQL. They run only under the
// "integration" build tag and skip unless UBIXVAULT_MARIADB_DSN is set:
//
//	docker run --rm -e MARIADB_ROOT_PASSWORD=root -e MARIADB_DATABASE=testdb -p 3306:3306 mariadb:11
//	UBIXVAULT_MARIADB_DSN='root:root@tcp(127.0.0.1:3306)/testdb' go test -tags integration ./internal/database/mariadb/
package mariadb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"testing"
	"time"

	sqldriver "github.com/go-sql-driver/mysql"

	"github.com/cwolsen7905/ubixvault/internal/database"
)

func adminDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("UBIXVAULT_MARIADB_DSN")
	if dsn == "" {
		t.Skip("set UBIXVAULT_MARIADB_DSN to run MariaDB integration tests")
	}
	return dsn
}

// userDSN derives a DSN for a specific user from the admin DSN.
func userDSN(t *testing.T, admin, user, pass string) string {
	t.Helper()
	cfg, err := sqldriver.ParseDSN(admin)
	if err != nil {
		t.Fatalf("parse admin DSN: %v", err)
	}
	cfg.User = user
	cfg.Passwd = pass
	return cfg.FormatDSN()
}

func canConnect(dsn string) bool {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return false
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return db.PingContext(ctx) == nil
}

// TestCreateUserRevokeUser exercises the full plugin against a real database: a
// generated user can connect after creation and cannot after revocation.
func TestCreateUserRevokeUser(t *testing.T) {
	ctx := context.Background()
	admin := adminDSN(t)

	p := New()
	if err := p.Initialize(ctx, admin); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	defer p.Close()

	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	username := "uv_it_" + hex.EncodeToString(suffix)
	password := "Str0ng-" + hex.EncodeToString(suffix)

	req := database.CreateUserRequest{
		Username:   username,
		Password:   password,
		Expiration: time.Now().Add(time.Hour),
		CreationStatements: []string{
			"CREATE USER '{{username}}'@'%' IDENTIFIED BY '{{password}}'",
			"GRANT SELECT ON *.* TO '{{username}}'@'%'",
		},
	}
	if err := p.CreateUser(ctx, req); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	// Ensure cleanup even if an assertion fails.
	defer func() { _ = p.RevokeUser(ctx, username) }()

	if !canConnect(userDSN(t, admin, username, password)) {
		t.Fatal("created user cannot connect")
	}

	if err := p.RevokeUser(ctx, username); err != nil {
		t.Fatalf("RevokeUser: %v", err)
	}
	if canConnect(userDSN(t, admin, username, password)) {
		t.Fatal("revoked user can still connect")
	}
}

// TestRevokeMissingUserIsNoError confirms revoking an absent user succeeds.
func TestRevokeMissingUserIsNoError(t *testing.T) {
	ctx := context.Background()
	p := New()
	if err := p.Initialize(ctx, adminDSN(t)); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	defer p.Close()

	if err := p.RevokeUser(ctx, "uv_it_definitely_absent"); err != nil {
		t.Fatalf("RevokeUser(absent): %v", err)
	}
}
