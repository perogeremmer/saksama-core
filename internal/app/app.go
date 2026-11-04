// Package app holds the wiring every frontend needs before it can call the core.
//
// The CLI, the interactive wizard, and (later) an agent all have to answer the
// same two questions: how do I reach MySQL, and where does the state directory
// live? Answering them here keeps that logic out of every command, and out of the
// core, which must not know how a connection was resolved.
package app

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/perogeremmer/saksama-core/internal/cred"
	"github.com/perogeremmer/saksama-core/internal/state"
)

// ConnectTimeout bounds the initial ping. A migration may take hours; connecting
// to a misconfigured host must not.
const ConnectTimeout = 15 * time.Second

// Connect opens and verifies a connection.
//
// The ping is deliberate: `sql.Open` never contacts the server, so without it a
// bad host or a wrong password would surface later, in the middle of a plan, as a
// much less clear error.
func Connect(c cred.Conn) (*sql.DB, error) {
	db, err := sql.Open("mysql", c.DSN())
	if err != nil {
		return nil, fmt.Errorf("opening connection: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), ConnectTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connecting to %s as %s: %w", c.RedactedDSN(), c.User, err)
	}
	return db, nil
}

// Store opens the state directory. An empty dir means the default location, so a
// frontend that has no --state-dir flag still works.
func Store(dir string) (*state.Store, error) {
	return state.NewStore(dir)
}
