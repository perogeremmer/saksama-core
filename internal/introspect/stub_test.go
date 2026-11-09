package introspect

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/perogeremmer/saksama-core/internal/model"
)

// A stub SQL server: each test hands in one function that answers queries, so
// the collector's reading of hostile shapes (NULL lag, unreadable peers) can be
// exercised without a live MySQL.
type stubDriver struct {
	answer func(query string) ([]string, [][]driver.Value, error)
}

type stubConn struct{ d *stubDriver }

func (d *stubDriver) Open(name string) (driver.Conn, error) { return stubConn{d}, nil }
func (c stubConn) Prepare(q string) (driver.Stmt, error) {
	return nil, errors.New("prepared statements are not part of this stub")
}
func (c stubConn) Close() error              { return nil }
func (c stubConn) Begin() (driver.Tx, error) { return nil, errors.New("no transactions here") }

func (c stubConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	cols, rows, err := c.d.answer(query)
	if err != nil {
		return nil, err
	}
	return &stubRows{cols: cols, rows: rows}, nil
}

func (c stubConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(0), nil
}

type stubRows struct {
	cols []string
	rows [][]driver.Value
	i    int
}

func (r *stubRows) Columns() []string { return r.cols }
func (r *stubRows) Close() error      { return nil }
func (r *stubRows) Next(dest []driver.Value) error {
	if r.i >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.i])
	r.i++
	return nil
}

var stubCounter int64

func openStub(t *testing.T, answer func(query string) ([]string, [][]driver.Value, error)) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("saksama-introspect-stub-%d", atomic.AddInt64(&stubCounter, 1))
	sql.Register(name, &stubDriver{answer: answer})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// A replica whose SQL thread is stopped reports Seconds_Behind_Master = NULL and
// Replica_SQL_Running = NO. Both facts must read as "not running": an IO thread
// alone ships events nobody applies, and NULL lag must never read as zero.
func TestReplicaWithStoppedSQLThreadIsNotRunning(t *testing.T) {
	db := openStub(t, func(query string) ([]string, [][]driver.Value, error) {
		if strings.Contains(query, "REPLICA STATUS") || strings.Contains(query, "SLAVE STATUS") {
			return []string{"Replica_IO_Running", "Replica_SQL_Running", "Seconds_Behind_Master"},
				[][]driver.Value{{"Yes", "No", nil}}, nil
		}
		return nil, nil, errors.New("unexpected query: " + query)
	})
	c := NewCollector(db, "shop", "orders")
	lag, configured, running := c.replica(context.Background())
	if !configured {
		t.Fatal("a server that answered must read as configured")
	}
	if running {
		t.Fatal("a stopped SQL thread must read as not running")
	}
	if lag != 0 {
		t.Fatalf("NULL lag must not become a number: %v", lag)
	}
}

// The healthy case must stay healthy, and the lag must still be read.
func TestReplicaHealthyCase(t *testing.T) {
	db := openStub(t, func(query string) ([]string, [][]driver.Value, error) {
		return []string{"Replica_IO_Running", "Replica_SQL_Running", "Seconds_Behind_Master"},
			[][]driver.Value{{"Yes", "Yes", "5"}}, nil
	})
	c := NewCollector(db, "shop", "orders")
	_, _, running := c.replica(context.Background())
	lag, configured, _ := c.replica(context.Background())
	if !running || !configured || lag != 5 {
		t.Fatalf("lag=%v configured=%v running=%v", lag, configured, running)
	}
}

func fkFixture() model.Table {
	return model.Table{
		Name: "orders",
		Columns: []model.Column{
			{Name: "customer_id", DataType: "int"},
			{Name: "note", DataType: "varchar", Charset: "utf8mb4", Collation: "utf8mb4_0900_ai_ci"},
		},
		ForeignKeys: []model.ForeignKey{{
			Name: "fk_customer", Columns: []string{"customer_id"},
			RefSchema: "shop", RefTable: "customers", RefColumns: []string{"id"},
		}, {
			Name: "fk_note", Columns: []string{"note"},
			RefSchema: "shop", RefTable: "notes", RefColumns: []string{"note"},
		}},
	}
}

// A peer table that cannot be read must surface as unverified, not as
// compatible: an INFO line built on nothing is a false promise.
func TestFKCharsetUnreadablePeerIsNotAffirmed(t *testing.T) {
	db := openStub(t, func(query string) ([]string, [][]driver.Value, error) {
		if strings.Contains(query, "information_schema.COLUMNS") {
			return nil, nil, errors.New("access denied")
		}
		return nil, nil, errors.New("unexpected query: " + query)
	})
	c := NewCollector(db, "shop", "orders")
	checks, err := c.FKCharset(context.Background(), fkFixture())
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 2 {
		t.Fatalf("expected one check per foreign key, got %d", len(checks))
	}
	for _, fk := range checks {
		if fk.Verified {
			t.Fatalf("constraint %s must not claim verification", fk.Constraint)
		}
		if fk.Compatible {
			t.Fatalf("constraint %s must not claim compatibility either", fk.Constraint)
		}
		if fk.Note == "" {
			t.Fatalf("constraint %s must say why it could not be verified", fk.Constraint)
		}
	}
}

// A constraint whose columns are not text was never compared at all: charset
// agreement cannot be claimed for it.
func TestFKCharsetWithoutComparablePairsIsNotAffirmed(t *testing.T) {
	db := openStub(t, func(query string) ([]string, [][]driver.Value, error) {
		return []string{"COLUMN_NAME", "COLUMN_TYPE", "DATA_TYPE", "CHARACTER_SET_NAME", "COLLATION_NAME"},
			[][]driver.Value{{"id", "int", "int", nil, nil}}, nil
	})
	c := NewCollector(db, "shop", "orders")
	tbl := model.Table{
		Name:    "orders",
		Columns: []model.Column{{Name: "customer_id", DataType: "int"}},
		ForeignKeys: []model.ForeignKey{{
			Name: "fk_customer", Columns: []string{"customer_id"},
			RefSchema: "shop", RefTable: "customers", RefColumns: []string{"id"},
		}},
	}
	checks, err := c.FKCharset(context.Background(), tbl)
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 1 || checks[0].Verified {
		t.Fatalf("a numeric FK pair must read as unverified: %+v", checks)
	}
}

// The real comparison still runs when both sides are readable and textual.
func TestFKCharsetComparesWhenItCan(t *testing.T) {
	db := openStub(t, func(query string) ([]string, [][]driver.Value, error) {
		return []string{"COLUMN_NAME", "COLUMN_TYPE", "DATA_TYPE", "CHARACTER_SET_NAME", "COLLATION_NAME"},
			[][]driver.Value{{"note", "varchar(64)", "varchar", "utf8mb4", "utf8mb4_0900_ai_ci"}}, nil
	})
	c := NewCollector(db, "shop", "orders")
	checks, err := c.FKCharset(context.Background(), fkFixture())
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, fk := range checks {
		if fk.Constraint != "fk_note" {
			continue
		}
		found = true
		if !fk.Verified || !fk.Compatible {
			t.Fatalf("a matching pair must be verified and compatible: %+v", fk)
		}
	}
	if !found {
		t.Fatal("the textual constraint was not checked")
	}
}
