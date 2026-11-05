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
	"strings"
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
		return nil, fmt.Errorf("connecting to %s as %s: %w%s", c.RedactedDSN(), c.User, err, tlsHint(err, c.TLS))
	}
	return db, nil
}

// tlsHint turns the least useful error in this program into one that says what to
// try. "remote error: tls: handshake failure" is what Go prints when the server's
// TLS stack and the client's do not agree - usually a server too old to negotiate
// anything this client accepts. Certificate verification is a different failure, and
// --tls skip-verify does not help with this one, so the hint names the difference and
// leaves the decision with the operator: disabling TLS is a security choice, not a
// workaround, and only the person who knows the network path can make it.
func tlsHint(err error, mode string) string {
	if err == nil || mode == "false" {
		return ""
	}
	msg := err.Error()
	if !strings.Contains(msg, "tls: ") && !strings.Contains(msg, "x509: ") {
		return ""
	}
	verified := strings.Contains(msg, "x509:")
	hint := "\nthe server offered TLS but the handshake failed. "
	if verified {
		hint += "That is the certificate (self-signed or expired): --tls skip-verify accepts it without checking the chain."
	} else {
		hint += "That is the protocol or cipher list, not the certificate: the server cannot negotiate anything this client accepts, so --tls skip-verify fails the same way."
	}
	hint += "\nIf this link is trusted (same host, an SSH tunnel, a local container), retry with --tls false."
	return hint
}

// Store opens the state directory. An empty dir means the default location, so a
// frontend that has no --state-dir flag still works.
func Store(dir string) (*state.Store, error) {
	return state.NewStore(dir)
}
