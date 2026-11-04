// Package introspect reads a live MySQL server into the neutral model the
// analyzer consumes.
//
// The analyzer never talks to a database; everything it needs arrives through
// this package. That separation is what keeps the decision layer unit-testable
// and keeps SQL quirks in one place.
package introspect

import (
	"context"
	"database/sql"

	"github.com/perogeremmer/saksama-core/internal/model"
)

// Querier is the subset of database/sql that introspection needs. Both *sql.DB
// and *sql.Tx satisfy it, so a snapshot can be taken inside a transaction when
// consistency matters.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// LongQuery is one running statement that could hold a metadata lock.
type LongQuery struct {
	ID      int64  `json:"id"`
	User    string `json:"user"`
	Host    string `json:"host"`
	Seconds int64  `json:"seconds"`
	State   string `json:"state"`
	SQL     string `json:"sql"` // truncated and redacted by the caller
}

// Transaction is one open InnoDB transaction.
type Transaction struct {
	ID           string `json:"id"`
	AgeSeconds   int64  `json:"age_seconds"`
	State        string `json:"state"`
	RowsLocked   int64  `json:"rows_locked"`
	RowsModified int64  `json:"rows_modified"`
	Tables       string `json:"tables"`
}

// Artifact is a leftover object from a previous migration attempt.
type Artifact struct {
	Kind     string `json:"kind"` // table | trigger
	Name     string `json:"name"`
	Evidence string `json:"evidence"`
}

// Grants summarises the privileges that matter for online DDL and copy engines.
type Grants struct {
	User             string   `json:"user"`
	Super            bool     `json:"super"`
	Process          bool     `json:"process"`
	Reload           bool     `json:"reload"`
	SystemVarsAdmin  bool     `json:"system_vars_admin"`
	SessionVarsAdmin bool     `json:"session_vars_admin"`
	BackupAdmin      bool     `json:"backup_admin"`
	RepClient        bool     `json:"replication_client"`
	AllPrivileges    bool     `json:"all_privileges"`
	Raw              []string `json:"raw"`
}

// Runtime is the live server state that affects a migration decision.
type Runtime struct {
	ThreadsRunning    int           `json:"threads_running"`
	LongQueries       []LongQuery   `json:"long_queries"`
	Transactions      []Transaction `json:"transactions"`
	PendingMDLocks    int           `json:"pending_metadata_locks"`
	ReplicaLagSec     float64       `json:"replica_lag_seconds"`
	ReplicaConfigured bool          `json:"replica_configured"`
	ReplicaRunning    bool          `json:"replica_running"`
	Grants            Grants        `json:"grants"`
	Artifacts         []Artifact    `json:"artifacts"`
	LockWaitTimeout   int64         `json:"lock_wait_timeout"`
	// Disk is the volume holding the data directory. MySQL cannot report free
	// filesystem space over SQL, so this is only measured when Saksama runs on the
	// database host.
	Disk    DiskSpace `json:"disk"`
	DataDir string    `json:"datadir,omitempty"`
}

// FKCheck is the verifiable part of check A1: the character set of both sides of
// a foreign key relationship.
type FKCheck struct {
	Constraint string         `json:"constraint"`
	Direction  string         `json:"direction"` // outgoing | incoming
	PeerTable  string         `json:"peer_table"`
	Pairs      []FKColumnPair `json:"pairs"`
	Compatible bool           `json:"compatible"`
}

// FKColumnPair compares one column on each side of a constraint.
type FKColumnPair struct {
	LocalColumn    string `json:"local_column"`
	LocalCharset   string `json:"local_charset"`
	LocalCollation string `json:"local_collation"`
	PeerColumn     string `json:"peer_column"`
	PeerCharset    string `json:"peer_charset"`
	PeerCollation  string `json:"peer_collation"`
	Compatible     bool   `json:"compatible"`
}

// Snapshot is everything a live plan needs in one struct.
type Snapshot struct {
	Server      model.Server     `json:"server"`
	Table       model.Table      `json:"table"`
	Schema      string           `json:"schema"`
	Runtime     Runtime          `json:"runtime"`
	FKChecks    []FKCheck        `json:"fk_checks"`
	Duplicates  map[string]int64 `json:"duplicates"` // index name -> duplicate groups found (capped)
	CollectedAt string           `json:"collected_at"`
}
