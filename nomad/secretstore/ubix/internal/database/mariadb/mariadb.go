// Package mariadb is the reference [database.Plugin] for MariaDB/MySQL
// (docs/DECISIONS.md D-006). It executes an operator-supplied creation statement
// to make a short-lived user and drops that user on revocation.
//
// This is the project's first third-party dependency: talking to MariaDB
// requires its wire protocol, so a vetted driver (go-sql-driver/mysql) is used
// rather than reimplementing it (docs/DECISIONS.md D-010). The "no hand-rolled
// cryptography" rule is unaffected — this is a database driver, not crypto.
package mariadb

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"

	_ "github.com/go-sql-driver/mysql" // registers the "mysql" driver

	"github.com/cwolsen7905/ubixvault/internal/database"
)

// mysqlTimeLayout is MariaDB's DATETIME format, used for {{expiration}}.
const mysqlTimeLayout = "2006-01-02 15:04:05"

// Plugin is a database.Plugin backed by a MariaDB/MySQL connection.
type Plugin struct {
	mu sync.RWMutex // guards db, which Initialize and Close replace
	db *sql.DB
}

// conn returns the current pool, or an error if the plugin is not initialized.
func (p *Plugin) conn() (*sql.DB, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.db == nil {
		return nil, fmt.Errorf("mariadb: not initialized")
	}
	return p.db, nil
}

// New returns an unconnected plugin. Call Initialize before use.
func New() *Plugin { return &Plugin{} }

// Compile-time check that Plugin satisfies the engine's interface.
var _ database.Plugin = (*Plugin)(nil)

// Initialize opens a connection pool using a go-sql-driver DSN
// ("user:pass@tcp(host:port)/db") and verifies it with a ping.
func (p *Plugin) Initialize(ctx context.Context, connectionURL string) error {
	db, err := sql.Open("mysql", connectionURL)
	if err != nil {
		return fmt.Errorf("mariadb: open: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return fmt.Errorf("mariadb: ping: %w", err)
	}
	p.mu.Lock()
	old := p.db
	p.db = db
	p.mu.Unlock()
	if old != nil {
		_ = old.Close() // reconfigured: release the previous pool
	}
	return nil
}

// CreateUser runs the role's creation statements with {{username}},
// {{password}}, and {{expiration}} substituted, inside a transaction.
//
// Identifiers and secrets cannot be passed as bound parameters in DDL, so the
// engine's generated username (hex) and password (base64url) — both free of SQL
// metacharacters — are substituted textually. This is inherent to the
// database-secrets pattern and matches HashiCorp Vault.
func (p *Plugin) CreateUser(ctx context.Context, req database.CreateUserRequest) error {
	db, err := p.conn()
	if err != nil {
		return err
	}
	replacer := strings.NewReplacer(
		"{{username}}", req.Username,
		"{{name}}", req.Username,
		"{{password}}", req.Password,
		"{{expiration}}", req.Expiration.UTC().Format(mysqlTimeLayout),
	)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mariadb: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, stmt := range req.CreationStatements {
		stmt = strings.TrimSpace(replacer.Replace(stmt))
		if stmt == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("mariadb: create user: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mariadb: commit: %w", err)
	}
	return nil
}

// RevokeUser drops the user across all hosts it was granted for, so the
// credential stops working regardless of the host clause in the creation
// statement.
func (p *Plugin) RevokeUser(ctx context.Context, username string) error {
	db, err := p.conn()
	if err != nil {
		return err
	}
	rows, err := db.QueryContext(ctx, "SELECT Host FROM mysql.user WHERE User = ?", username)
	if err != nil {
		return fmt.Errorf("mariadb: find user hosts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var hosts []string
	for rows.Next() {
		var host string
		if err := rows.Scan(&host); err != nil {
			return fmt.Errorf("mariadb: scan host: %w", err)
		}
		hosts = append(hosts, host)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("mariadb: iterate hosts: %w", err)
	}

	for _, host := range hosts {
		// username and host come from mysql.user, not user input; quote defensively.
		stmt := fmt.Sprintf("DROP USER IF EXISTS '%s'@'%s'",
			strings.ReplaceAll(username, "'", "''"),
			strings.ReplaceAll(host, "'", "''"))
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("mariadb: drop user: %w", err)
		}
	}
	return nil
}

// Close closes the underlying connection pool.
func (p *Plugin) Close() error {
	p.mu.Lock()
	db := p.db
	p.db = nil
	p.mu.Unlock()
	if db == nil {
		return nil
	}
	return db.Close()
}
