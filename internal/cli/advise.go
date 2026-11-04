package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
	"github.com/perogeremmer/saksama-core/internal/core"
	"github.com/perogeremmer/saksama-core/internal/introspect"
	"github.com/perogeremmer/saksama-core/internal/model"
	"github.com/perogeremmer/saksama-core/internal/parser"
	"github.com/perogeremmer/saksama-core/internal/probe"
	"github.com/perogeremmer/saksama-core/internal/render"
	"github.com/perogeremmer/saksama-core/internal/state"
)

type adviseOpts struct {
	connFlags

	mysqlVersion string

	// offline source
	ddlPath    string
	schemaPath string

	table      string
	alter      string
	production bool
	engine     string
	jsonOut    bool
	noProbe    bool
	plain      bool
	rows       int64
	dataBytes  int64
	indexBytes int64
}

func cmdAdvise(args []string, stdout, stderr io.Writer) int {
	var o adviseOpts

	live := hasFlag(args, "--live")
	args = removeFlag(args, "--live")

	fs := flag.NewFlagSet("advise", flag.ContinueOnError)
	fs.SetOutput(stderr)

	if live {
		addConnFlags(fs, &o.connFlags)
	} else {
		fs.StringVar(&o.ddlPath, "ddl", "", "SHOW CREATE TABLE output, or - for stdin")
		fs.StringVar(&o.schemaPath, "schema", "", "table JSON produced by plan --dump-schema")
		// Accepted offline too, so a recorded engine choice can be reported.
		fs.StringVar(&o.database, "d", "", "database (for the recorded engine choice)")
		fs.StringVar(&o.database, "database", "", "database")
	}

	fs.StringVar(&o.table, "t", "", "table (also: --table)")
	fs.StringVar(&o.table, "table", "", "table")
	fs.StringVar(&o.alter, "a", "", "ALTER statement (also: --alter)")
	fs.StringVar(&o.alter, "alter", "", "ALTER statement")
	fs.StringVar(&o.mysqlVersion, "mysql-version", "", "server version (offline mode)")
	fs.BoolVar(&o.production, "production", false, "mark the target as production")
	fs.StringVar(&o.engine, "engine", "", "force native|gh-ost|pt-osc when planning")
	fs.BoolVar(&o.jsonOut, "json", false, "JSON output")
	fs.BoolVar(&o.noProbe, "no-probe", false, "skip probing installed engine binaries")
	fs.BoolVar(&o.plain, "plain", false, "borderless output, for scripts and logs")
	fs.Int64Var(&o.rows, "rows", 0, "estimated row count")
	fs.Int64Var(&o.dataBytes, "data-bytes", 0, "table data bytes")
	fs.Int64Var(&o.indexBytes, "index-bytes", 0, "table index bytes")
	fs.Bool("no-color", false, "accepted for compatibility")

	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}

	var binaries map[string]analyzer.EngineBinary
	if !o.noProbe {
		binaries = probe.Binaries()
	}

	if live {
		return adviseLive(o, binaries, stdout, stderr)
	}
	return adviseOffline(o, binaries, stdout, stderr)
}

func applyEngineHint(o adviseOpts, in *analyzer.Input) int {
	if o.engine == "" {
		return 0
	}
	e, err := parseEngine(o.engine)
	if err != nil {
		return ExitUsage
	}
	in.EngineHint = e
	return 0
}

func adviseOffline(o adviseOpts, binaries map[string]analyzer.EngineBinary, stdout, stderr io.Writer) int {
	var (
		tbl     model.Table
		srv     model.Server
		src     string
		ignored []string
	)

	switch {
	case o.schemaPath != "":
		data, err := os.ReadFile(o.schemaPath)
		if err != nil {
			fmt.Fprintf(stderr, "saksama advise: %v\n", err)
			return ExitUsage
		}
		var payload schemaFile
		if err := json.Unmarshal(data, &payload); err != nil {
			fmt.Fprintf(stderr, "saksama advise: %s is not a schema file: %v\n", o.schemaPath, err)
			return ExitUsage
		}
		tbl, srv, src = payload.Table, payload.Server, "json"

	case o.ddlPath != "":
		ddl, err := readInput(o.ddlPath)
		if err != nil {
			fmt.Fprintf(stderr, "saksama advise: %v\n", err)
			return ExitUsage
		}
		parsed, ign, err := parser.ParseCreateTable(ddl)
		if err != nil {
			fmt.Fprintf(stderr, "saksama advise: cannot parse table definition: %v\n", err)
			return ExitUsage
		}
		tbl, ignored, src = parsed, ign, "ddl"

	default:
		fmt.Fprintln(stderr, "saksama advise: give --ddl/--schema, or use --live")
		return ExitUsage
	}

	if o.mysqlVersion != "" {
		srv.Version = o.mysqlVersion
	}
	if srv.Vendor == "" {
		srv.Vendor = "mysql"
	}
	if o.table != "" {
		tbl.Name = o.table
	}
	if o.rows > 0 {
		tbl.Rows = o.rows
	}
	if o.dataBytes > 0 {
		tbl.DataLength = o.dataBytes
	}
	if o.indexBytes > 0 {
		tbl.IndexLength = o.indexBytes
	}

	var ops []model.Op
	if strings.TrimSpace(o.alter) != "" {
		alter, err := parser.ParseAlter("ALTER TABLE `" + strings.ReplaceAll(tbl.Name, "`", "``") + "` " + alterStatementBody(o.alter))
		ops = alter.Ops
		if err != nil {
			ignored = append(ignored, "ALTER statement could not be parsed: "+err.Error())
			ops = []model.Op{{Kind: model.OpUnknown, Raw: strings.TrimSpace(o.alter)}}
		}
	} else {
		rows := analyzer.AssessSchema([]model.Table{tbl}, srv, binaries)
		if o.jsonOut {
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(map[string]any{"schema": tbl.Schema, "server": srv.Version,
				"tables": rows, "artifacts": introspect.FindArtifacts([]model.Table{tbl})})
			return ExitOK
		}
		if err := render.SchemaReportStyled(stdout, tbl.Schema, rows, introspect.FindArtifacts([]model.Table{tbl}), !o.plain); err != nil {
			fmt.Fprintf(stderr, "saksama advise: %v\n", err)
			return ExitInternal
		}
		return ExitOK
	}

	in := analyzer.Input{
		Server:       srv,
		Table:        tbl,
		Ops:          ops,
		RawAlter:     strings.TrimSpace(o.alter),
		Host:         o.host,
		Production:   o.production,
		SchemaSource: src,
		Ignored:      ignored,
		Binaries:     binaries,
	}
	if code := applyEngineHint(o, &in); code != 0 {
		fmt.Fprintf(stderr, "saksama advise: unknown engine %q\n", o.engine)
		return code
	}

	choice, _ := loadEngineChoice(o.database, o.table)
	plan := analyzer.Analyze(in)
	return writeAdvise(o, plan, chosenLabel(choice), stdout, stderr)
}

// loadEngineChoice reads the recorded decision for a target, and falls back to
// the default choice.
func loadEngineChoice(database, table string) (*state.EngineChoice, error) {
	store, err := newStore()
	if err != nil {
		return nil, err
	}
	return store.ResolveEngine(database, table)
}

func chosenLabel(c *state.EngineChoice) string {
	if c == nil {
		return ""
	}
	return c.Engine
}

func adviseLive(o adviseOpts, binaries map[string]analyzer.EngineBinary, stdout, stderr io.Writer) int {
	if o.database == "" {
		fmt.Fprintln(stderr, "saksama advise --live: --database is required")
		return ExitUsage
	}
	// No table name, or no ALTER: report table shape instead of demanding a change.
	// The operator should not have to name one just to ask about the engine.
	if o.table == "" || strings.TrimSpace(o.alter) == "" {
		return adviseSchema(o, binaries, stdout, stderr)
	}

	conn, err := o.resolve()
	if err != nil {
		fmt.Fprintf(stderr, "saksama advise: %v\n", err)
		return ExitUsage
	}
	db, err := openDB(conn)
	if err != nil {
		fmt.Fprintf(stderr, "saksama advise: %v\n", err)
		return ExitInternal
	}
	defer db.Close()

	store, err := newStore()
	if err != nil {
		fmt.Fprintf(stderr, "saksama advise: %v\n", err)
		return ExitInternal
	}

	runner := core.New(db, store)
	req := core.Request{
		Conn:       conn,
		Database:   o.database,
		Table:      o.table,
		Alter:      o.alter,
		Production: o.production,
		Actor:      currentActor(),
		Binaries:   binaries,
	}
	if o.engine != "" {
		e, err := parseEngine(o.engine)
		if err != nil {
			fmt.Fprintf(stderr, "saksama advise: %v\n", err)
			return ExitUsage
		}
		req.EngineHint = e
	}

	prep, err := runner.PlanOnly(context.Background(), req)
	if err != nil {
		fmt.Fprintf(stderr, "saksama advise: %v\n", err)
		return ExitInternal
	}
	defer prep.Close()

	choice, _ := loadEngineChoice(o.database, o.table)
	return writeAdvise(o, prep.Plan, chosenLabel(choice), stdout, stderr)
}

func writeAdvise(o adviseOpts, plan analyzer.Plan, chosen string, stdout, stderr io.Writer) int {
	if o.jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(struct {
			analyzer.Plan
			Recommended analyzer.Engine `json:"recommended"`
		}{Plan: plan, Recommended: plan.Recommendation.Engine}); err != nil {
			fmt.Fprintf(stderr, "saksama advise: %v\n", err)
			return ExitInternal
		}
	} else {
		if err := render.EngineMatrix(stdout, plan, chosen); err != nil {
			fmt.Fprintf(stderr, "saksama advise: %v\n", err)
			return ExitInternal
		}
	}

	if plan.Recommendation.Blockers > 0 {
		return ExitBlocked
	}
	return ExitOK
}

// adviseSchema reads every table in the database and reports engine fitness.
//
// This is the mode people actually want first: they know the database, not yet
// which table and which engine. Inspecting it for them is the point.
func adviseSchema(o adviseOpts, binaries map[string]analyzer.EngineBinary, stdout, stderr io.Writer) int {
	conn, err := o.resolve()
	if err != nil {
		fmt.Fprintf(stderr, "saksama advise: %v\n", err)
		return ExitUsage
	}
	db, err := openDB(conn)
	if err != nil {
		fmt.Fprintf(stderr, "saksama advise: %v\n", err)
		return ExitInternal
	}
	defer db.Close()

	ctx := context.Background()
	collector := introspect.NewCollector(db, o.database, o.table)
	srv, err := collector.Server(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "saksama advise: %v\n", err)
		return ExitInternal
	}
	var tables []model.Table
	if o.table != "" {
		tbl, err := collector.Table(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "saksama advise: %v\n", err)
			return ExitInternal
		}
		tables = append(tables, tbl)
	} else {
		tables, err = collector.SchemaTables(ctx, false)
		if err != nil {
			fmt.Fprintf(stderr, "saksama advise: %v\n", err)
			return ExitInternal
		}
	}

	rows := analyzer.AssessSchema(tables, srv, binaries)
	artifacts := introspect.FindArtifacts(tables)

	if o.jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"schema":    o.database,
			"server":    srv.Version,
			"tables":    rows,
			"artifacts": artifacts,
		})
		return ExitOK
	}

	if err := render.SchemaReportStyled(stdout, o.database, rows, artifacts, !o.plain); err != nil {
		fmt.Fprintf(stderr, "saksama advise: %v\n", err)
		return ExitInternal
	}
	return ExitOK
}
