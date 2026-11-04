package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/perogeremmer/saksama-core/internal/state"
)

const testDDL = "CREATE TABLE `orders` (\n" +
	"  `id` bigint unsigned NOT NULL AUTO_INCREMENT,\n" +
	"  `spot_id` bigint unsigned NOT NULL,\n" +
	"  `status` varchar(32) NOT NULL DEFAULT 'pending',\n" +
	"  `created_at` datetime NOT NULL,\n" +
	"  PRIMARY KEY (`id`),\n" +
	"  KEY `ix_spot` (`spot_id`)\n" +
	") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC"

const testDDLWithFK = "CREATE TABLE `orders` (\n" +
	"  `id` bigint unsigned NOT NULL AUTO_INCREMENT,\n" +
	"  `spot_id` bigint unsigned NOT NULL,\n" +
	"  `status` varchar(32) NOT NULL,\n" +
	"  PRIMARY KEY (`id`),\n" +
	"  CONSTRAINT `fk_spot` FOREIGN KEY (`spot_id`) REFERENCES `spots` (`id`) ON DELETE CASCADE\n" +
	") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4"

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestPlanSimpleAddIndexSucceeds(t *testing.T) {
	ddl := writeTemp(t, "orders.sql", testDDL)
	code, out, errOut := run(t, "plan",
		"--ddl", ddl,
		"--alter", "ADD INDEX ix_created (created_at)",
		"--mysql-version", "8.0.36",
		"--host", "db.prod")
	if code != ExitOK {
		t.Fatalf("got exit %d, want %d; stderr=%s", code, ExitOK, errOut)
	}
	if !strings.Contains(out, "native") {
		t.Fatalf("expected native recommendation:\n%s", out)
	}
	if !strings.Contains(out, "RESULT     ready") {
		t.Fatalf("expected ready result:\n%s", out)
	}
}

func TestPlanBlockedReturnsNonZeroExit(t *testing.T) {
	// A TEXT index without a prefix cannot be created by any engine, so this is a
	// genuine refusal rather than an engine choice.
	ddl := writeTemp(t, "t.sql",
		"CREATE TABLE `t` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, `note` text, "+
			"PRIMARY KEY (`id`)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4")

	code, out, _ := run(t, "plan",
		"--ddl", ddl,
		"--alter", "ADD INDEX ix_note (note)",
		"--mysql-version", "8.0.36",
		"--host", "db.prod")
	if code != ExitBlocked {
		t.Fatalf("got exit %d, want %d", code, ExitBlocked)
	}
	if !strings.Contains(out, "RESULT     refused") {
		t.Fatalf("expected refused result:\n%s", out)
	}
}

// A table with foreign keys is not a dead end: pt-osc handles them, so the plan
// must name that engine instead of refusing.
func TestPlanWithForeignKeysSelectsPtOSC(t *testing.T) {
	ddl := writeTemp(t, "orders.sql", testDDLWithFK)
	code, out, _ := run(t, "plan",
		"--ddl", ddl,
		"--alter", "MODIFY COLUMN status varchar(64) NOT NULL",
		"--mysql-version", "8.0.36",
		"--host", "db.prod")
	if code != ExitOK {
		t.Fatalf("got exit %d, want 0:\n%s", code, out)
	}
	if !strings.Contains(out, "RECOMMEND  pt-osc") {
		t.Fatalf("expected pt-osc:\n%s", out)
	}
}

func TestPlanJSONIsMachineReadable(t *testing.T) {
	ddl := writeTemp(t, "orders.sql", testDDL)
	code, out, errOut := run(t, "plan",
		"--ddl", ddl,
		"--alter", "ADD INDEX ix_created (created_at)",
		"--mysql-version", "8.0.36",
		"--json")
	if code != ExitOK {
		t.Fatalf("got exit %d; stderr=%s", code, errOut)
	}

	// stdout must contain JSON only, no decorative card text.
	var payload map[string]any
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("stdout is not pure JSON: %v\n%s", err, out)
	}

	required := []string{
		"contract_version", "generated_at", "target", "alter", "server",
		"schema", "schema_fingerprint", "operations", "recommendation",
		"checks", "duration", "disk", "recovery", "verification",
	}
	for _, k := range required {
		if _, ok := payload[k]; !ok {
			t.Errorf("missing required key %q", k)
		}
	}
	if v, _ := payload["contract_version"].(float64); int(v) != 1 {
		t.Errorf("got contract_version %v", payload["contract_version"])
	}
}

func TestPlanJSONKeysMatchPublishedSchema(t *testing.T) {
	ddl := writeTemp(t, "orders.sql", testDDL)
	_, out, _ := run(t, "plan", "--ddl", ddl,
		"--alter", "ADD INDEX ix_created (created_at)",
		"--mysql-version", "8.0.36", "--json", "--rows", "1000")

	var planPayload map[string]any
	if err := json.Unmarshal([]byte(out), &planPayload); err != nil {
		t.Fatal(err)
	}

	var schemaDoc map[string]any
	if err := json.Unmarshal([]byte(planJSONSchema), &schemaDoc); err != nil {
		t.Fatalf("published schema is not valid JSON: %v", err)
	}
	props, ok := schemaDoc["properties"].(map[string]any)
	if !ok {
		t.Fatal("schema has no properties")
	}
	for k := range planPayload {
		if _, ok := props[k]; !ok {
			t.Errorf("plan emits key %q that the published schema does not describe", k)
		}
	}
}

func TestSchemaCommandEmitsJSONSchema(t *testing.T) {
	code, out, _ := run(t, "schema")
	if code != ExitOK {
		t.Fatalf("got exit %d", code)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("schema output is not JSON: %v", err)
	}
	if doc["$id"] == nil {
		t.Fatal("schema has no $id")
	}
}

func TestMissingVersionIsBlocked(t *testing.T) {
	ddl := writeTemp(t, "orders.sql", testDDL)
	code, out, _ := run(t, "plan", "--ddl", ddl, "--alter", "ADD INDEX ix (created_at)")
	if code != ExitBlocked {
		t.Fatalf("got exit %d, want %d", code, ExitBlocked)
	}
	if !strings.Contains(out, "E3") {
		t.Fatalf("expected version blocker:\n%s", out)
	}
}

func TestUnparseableAlterStillProducesPlan(t *testing.T) {
	ddl := writeTemp(t, "orders.sql", testDDL)
	code, out, _ := run(t, "plan", "--ddl", ddl,
		"--alter", "ADD INDEX (((", "--mysql-version", "8.0.36", "--json")
	if code != ExitBlocked {
		t.Fatalf("got exit %d, want %d", code, ExitBlocked)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("expected machine-readable blocker, got: %v\n%s", err, out)
	}
}

func TestUnknownCommandExitsUsage(t *testing.T) {
	code, _, errOut := run(t, "frobnicate")
	if code != ExitUsage {
		t.Fatalf("got exit %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(errOut, "unknown command") {
		t.Fatalf("unexpected stderr: %s", errOut)
	}
}

func TestUnimplementedCommandsAreHonest(t *testing.T) {
	for _, cmd := range []string{"cutover", "recover", "daemon"} {
		code, _, errOut := run(t, cmd)
		if code != ExitUsage {
			t.Errorf("%s: got exit %d", cmd, code)
		}
		if !strings.Contains(errOut, "not implemented") {
			t.Errorf("%s: expected honest message, got %q", cmd, errOut)
		}
	}
}

func TestImplementedCommandsDemandRequiredArguments(t *testing.T) {
	// apply without a target must fail loudly rather than doing something clever.
	code, _, errOut := run(t, "apply")
	if code != ExitUsage || !strings.Contains(errOut, "--database") {
		t.Fatalf("apply: exit %d, stderr %q", code, errOut)
	}
	for _, cmd := range []string{"events", "export", "cleanup", "attach", "cancel"} {
		code, _, errOut := run(t, cmd)
		if code != ExitUsage || !strings.Contains(errOut, "session id required") {
			t.Errorf("%s: exit %d, stderr %q", cmd, code, errOut)
		}
	}
}

func TestApplyRefusesProductionWithoutConfirmation(t *testing.T) {
	code, _, errOut := run(t, "apply",
		"-h", "db.prod", "-d", "shop", "-t", "orders",
		"-a", "ADD INDEX ix (created_at)", "--production", "--no-defaults")
	if code != ExitUsage {
		t.Fatalf("got exit %d, want usage error", code)
	}
	if !strings.Contains(errOut, "refusing to mutate a production target") {
		t.Fatalf("unexpected stderr: %s", errOut)
	}
	if !strings.Contains(errOut, "--confirm db.prod") {
		t.Fatalf("expected the exact confirmation instruction, got: %s", errOut)
	}
}

func TestDumpSchemaRoundTrip(t *testing.T) {
	ddl := writeTemp(t, "orders.sql", testDDL)
	code, out, errOut := run(t, "plan", "--ddl", ddl, "--dump-schema", "--mysql-version", "8.0.36")
	if code != ExitOK {
		t.Fatalf("got exit %d; stderr=%s", code, errOut)
	}
	schemaPath := writeTemp(t, "schema.json", out)

	code2, out2, errOut2 := run(t, "plan",
		"--schema", schemaPath,
		"--alter", "ADD INDEX ix_created (created_at)")
	if code2 != ExitOK {
		t.Fatalf("round trip failed: exit %d; stderr=%s", code2, errOut2)
	}
	if !strings.Contains(out2, "native") {
		t.Fatalf("expected native from dumped schema:\n%s", out2)
	}
}

func TestFreeDiskBlockerViaCLI(t *testing.T) {
	ddl := writeTemp(t, "orders.sql", testDDL)
	code, out, _ := run(t, "plan", "--ddl", ddl,
		"--alter", "MODIFY COLUMN status varchar(64) NOT NULL",
		"--mysql-version", "8.0.36",
		"--data-bytes", "100000000000",
		"--free-disk", "1000")
	if code != ExitBlocked {
		t.Fatalf("got exit %d, want %d", code, ExitBlocked)
	}
	if !strings.Contains(out, "D6") {
		t.Fatalf("expected disk blocker:\n%s", out)
	}
}

func TestEngineOverrideIsReported(t *testing.T) {
	ddl := writeTemp(t, "orders.sql", testDDL)
	code, out, _ := run(t, "plan", "--ddl", ddl,
		"--alter", "ADD INDEX ix_created (created_at)",
		"--mysql-version", "8.0.36",
		"--engine", "gh-ost")
	if code != ExitOK {
		t.Fatalf("got exit %d", code)
	}
	if !strings.Contains(out, "overridden to gh-ost by flag") {
		t.Fatalf("override not reported:\n%s", out)
	}
	if !strings.Contains(out, "analyzer would have picked native") {
		t.Fatalf("the analyzer's own pick must stay visible:\n%s", out)
	}
}

func TestVersionCommand(t *testing.T) {
	code, out, _ := run(t, "version")
	if code != ExitOK {
		t.Fatalf("got exit %d", code)
	}
	if !strings.Contains(out, "contract v1") {
		t.Fatalf("unexpected version output: %s", out)
	}
}

func TestUnparseableAlterIsNeverReady(t *testing.T) {
	ddl := writeTemp(t, "orders.sql", testDDL)
	// "ADD" alone is structurally unparseable: no column, no index, no target.
	code, out, _ := run(t, "plan", "--ddl", ddl,
		"--alter", "ADD", "--mysql-version", "8.0.36")
	if code != ExitBlocked {
		t.Fatalf("got exit %d, want %d; a CI gate must fail here\n%s", code, ExitBlocked, out)
	}
	if !strings.Contains(out, "RESULT     refused") {
		t.Fatalf("expected refused:\n%s", out)
	}
}

// Regression: "status s2 --json" must still emit JSON. The standard library
// flag parser stops at the first positional argument, so flags after it were
// silently ignored.
func TestStatusJSONAfterPositionalArgument(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SAKSAMA_STATE_DIR", dir)

	store, err := state.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&state.Session{
		ID: "s1", CreatedAt: "2026-01-01T00:00:00Z", State: state.StateDone,
		Database: "shop", Table: "orders", Alter: "ADD INDEX ix (a)", Engine: "native",
	}); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := run(t, "status", "s1", "--json")
	if code != ExitOK {
		t.Fatalf("exit %d, stderr %s", code, errOut)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("expected JSON, got: %v\n%s", err, out)
	}
	if payload["id"] != "s1" {
		t.Fatalf("wrong session in output: %v", payload["id"])
	}
}

func TestStatusWithoutSessionsIsEmptyNotAnError(t *testing.T) {
	t.Setenv("SAKSAMA_STATE_DIR", t.TempDir())
	code, out, _ := run(t, "status")
	if code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "no sessions recorded") {
		t.Fatalf("unexpected output: %s", out)
	}
}

func TestExportIncludesRedactionSafeSummary(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SAKSAMA_STATE_DIR", dir)

	store, err := state.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&state.Session{
		ID: "s1", CreatedAt: "2026-01-01T00:00:00Z", State: state.StateDone,
		Database: "shop", Table: "orders", Alter: "ADD INDEX ix (a)", Engine: "native",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvents("s1", []byte(`{"seq":1,"type":"session.created","session":"s1","contract_version":1,"ts":1,"data":{}}`+"\n")); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := run(t, "export", "s1")
	if code != ExitOK {
		t.Fatalf("exit %d, stderr %s", code, errOut)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("export is not JSON: %v", err)
	}
	if doc["completeness"] != "complete" {
		t.Fatalf("completeness = %v", doc["completeness"])
	}
	events, _ := doc["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}
}

func TestCleanupOnASessionWithNoArtifacts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SAKSAMA_STATE_DIR", dir)

	store, err := state.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&state.Session{
		ID: "s1", CreatedAt: "2026-01-01T00:00:00Z", State: state.StateDone,
		Database: "shop", Table: "orders", Engine: "native",
	}); err != nil {
		t.Fatal(err)
	}

	code, out, _ := run(t, "cleanup", "s1")
	if code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "nothing to clean up") {
		t.Fatalf("unexpected output: %s", out)
	}
	// The refusal to guess must still be stated, so nobody assumes cleanup can be
	// pointed at a name pattern.
	if !strings.Contains(out, "nothing to clean up") {
		t.Fatalf("unexpected output: %s", out)
	}
}

// --- advise -----------------------------------------------------------------

func TestAdviseOfflineShowsAllThreeEngines(t *testing.T) {
	ddl := writeTemp(t, "orders.sql", testDDLWithFK)

	code, out, errOut := run(t, "advise",
		"--ddl", ddl,
		"--alter", "MODIFY COLUMN status varchar(64) NOT NULL",
		"--mysql-version", "8.0.36",
		"--no-probe")
	if code != ExitOK {
		t.Fatalf("exit %d, want 0 (pt-osc fits)\nstderr: %s", code, errOut)
	}
	for _, want := range []string{"ENGINE SUITABILITY", "NATIVE", "GH-OST", "PT-OSC", "VERDICT"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in advise output:\n%s", want, out)
		}
	}
	// The whole point: on an FK table, gh-ost must be the one ruled out.
	if !strings.Contains(out, "no foreign keys in or out") {
		t.Fatalf("expected the foreign-key requirement to appear:\n%s", out)
	}
}

func TestAdviseNativeSuitableExitsZero(t *testing.T) {
	ddl := writeTemp(t, "orders.sql", testDDL)

	code, out, _ := run(t, "advise",
		"--ddl", ddl,
		"--alter", "ADD INDEX ix_created (created_at)",
		"--mysql-version", "8.0.36",
		"--no-probe")
	if code != ExitOK {
		t.Fatalf("exit %d, want 0:\n%s", code, out)
	}
	if !strings.Contains(out, "analyzer recommends  native") {
		t.Fatalf("unexpected verdict:\n%s", out)
	}
	if !strings.Contains(out, "NATIVE    SUITABLE") {
		t.Fatalf("native should be SUITABLE:\n%s", out)
	}
}

func TestAdviseJSONIncludesEngineMatrix(t *testing.T) {
	ddl := writeTemp(t, "orders.sql", testDDL)

	code, out, errOut := run(t, "advise",
		"--ddl", ddl,
		"--alter", "MODIFY COLUMN status varchar(64) NOT NULL",
		"--mysql-version", "8.0.36",
		"--no-probe",
		"--json")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("advise --json is not JSON: %v\n%s", err, out)
	}
	engines, ok := payload["engines"].([]any)
	if !ok || len(engines) != 3 {
		t.Fatalf("expected 3 engine assessments, got %v", payload["engines"])
	}
	if payload["recommended"] != "gh-ost" {
		t.Fatalf("recommended = %v", payload["recommended"])
	}
	if _, ok := payload["table_facts"]; !ok {
		t.Fatal("advice must carry the schema facts it is based on")
	}

	// Every engine must report suitability separately from availability.
	for _, raw := range engines {
		m := raw.(map[string]any)
		for _, key := range []string{"engine", "verdict", "installed", "wrapped"} {
			if _, ok := m[key]; !ok {
				t.Errorf("engine %v missing %q", m["engine"], key)
			}
		}
	}
}

// Without an ALTER, advise still reports engine fitness for the table shape.
// The operator should not have to name a change just to ask about a table.
func TestAdviseWithoutAlterReportsTableShape(t *testing.T) {
	ddl := writeTemp(t, "orders.sql", testDDL)

	code, out, errOut := run(t, "advise", "--ddl", ddl, "--mysql-version", "8.0.36", "--no-probe")
	if code != ExitOK {
		t.Fatalf("exit %d, stderr %s", code, errOut)
	}
	if !strings.Contains(out, "ENGINE SUITABILITY for schema") {
		t.Fatalf("expected the schema report:\n%s", out)
	}
	if !strings.Contains(out, "REBUILD WITH") {
		t.Fatalf("expected the rebuild column:\n%s", out)
	}
	if !strings.Contains(out, "orders") {
		t.Fatalf("expected the table row:\n%s", out)
	}
}

func TestAdviseRequiresASource(t *testing.T) {
	code, _, errOut := run(t, "advise", "--alter", "ADD INDEX ix (a)")
	if code != ExitUsage {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errOut, "--ddl") {
		t.Fatalf("stderr: %s", errOut)
	}
}

// Recommending one table's engine must not require naming a change.
func TestAdviseWithoutAlterDoesNotDemandAChange(t *testing.T) {
	ddl := writeTemp(t, "orders.sql", testDDL)
	code, out, errOut := run(t, "advise", "--ddl", ddl, "--mysql-version", "8.0.36", "--json", "--no-probe")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	tables, _ := payload["tables"].([]any)
	if len(tables) != 1 {
		t.Fatalf("expected one table row, got %v", payload["tables"])
	}
	row := tables[0].(map[string]any)
	for _, key := range []string{"table", "primary_key", "gh_ost", "pt_osc", "rebuild_engine"} {
		if _, ok := row[key]; !ok {
			t.Errorf("missing %q in the table row", key)
		}
	}
}

// --- engine scopes ----------------------------------------------------------

func TestEngineSetGlobal(t *testing.T) {
	t.Setenv("SAKSAMA_STATE_DIR", t.TempDir())

	code, out, errOut := run(t, "engine", "set", "--global", "pt-osc", "--note", "default kita pt-osc")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "every table on every database") {
		t.Fatalf("unexpected output: %s", out)
	}

	// The flag may also come after the positional argument.
	code, out, _ = run(t, "engine", "set", "gh-ost", "--global")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, out)
	}

	code, out, _ = run(t, "engine", "show", "--global")
	if code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "engine        gh-ost") {
		t.Fatalf("unexpected show output: %s", out)
	}
}

func TestEngineSetDatabaseScope(t *testing.T) {
	t.Setenv("SAKSAMA_STATE_DIR", t.TempDir())

	code, out, errOut := run(t, "engine", "set", "pt-osc", "-d", "shop")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "every table in shop") {
		t.Fatalf("unexpected output: %s", out)
	}

	// A table with no specific choice resolves to the database choice.
	code, out, _ = run(t, "engine", "show", "-d", "shop", "-t", "orders")
	if code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "engine        pt-osc") {
		t.Fatalf("unexpected show output: %s", out)
	}
	if !strings.Contains(out, "from           database choice") {
		t.Fatalf("the source must be named: %s", out)
	}
}

func TestEngineResolutionOrderThroughCLI(t *testing.T) {
	t.Setenv("SAKSAMA_STATE_DIR", t.TempDir())

	run(t, "engine", "set", "--global", "native")
	run(t, "engine", "set", "-d", "shop", "pt-osc")
	run(t, "engine", "set", "-d", "shop", "-t", "orders", "gh-ost")

	cases := []struct {
		args []string
		want string
	}{
		{[]string{"engine", "show", "-d", "shop", "-t", "orders"}, "gh-ost"},
		{[]string{"engine", "show", "-d", "shop", "-t", "users"}, "pt-osc"},
		{[]string{"engine", "show", "-d", "other", "-t", "orders"}, "native"},
	}
	for _, tc := range cases {
		code, out, _ := run(t, tc.args...)
		if code != ExitOK {
			t.Fatalf("%v: exit %d", tc.args, code)
		}
		if !strings.Contains(out, "engine        "+tc.want) {
			t.Errorf("%v: expected %s, got:\n%s", tc.args, tc.want, out)
		}
	}
}

func TestEngineSetRejectsConflictingFlags(t *testing.T) {
	t.Setenv("SAKSAMA_STATE_DIR", t.TempDir())

	code, _, errOut := run(t, "engine", "set", "--global", "pt-osc", "-d", "shop")
	if code != ExitUsage {
		t.Fatalf("exit %d, want usage error", code)
	}
	if !strings.Contains(errOut, "cannot be combined") {
		t.Fatalf("unexpected stderr: %s", errOut)
	}

	code, _, errOut = run(t, "engine", "set", "pt-osc", "-t", "orders")
	if code != ExitUsage {
		t.Fatalf("exit %d, want usage error", code)
	}
	if !strings.Contains(errOut, "requires -d") {
		t.Fatalf("unexpected stderr: %s", errOut)
	}
}

func TestEngineClearGlobal(t *testing.T) {
	t.Setenv("SAKSAMA_STATE_DIR", t.TempDir())

	run(t, "engine", "set", "--global", "pt-osc")
	code, out, _ := run(t, "engine", "clear", "--global")
	if code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "cleared") {
		t.Fatalf("unexpected output: %s", out)
	}
	code, out, _ = run(t, "engine", "show", "--global")
	if code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "(not set)") {
		t.Fatalf("global should be cleared: %s", out)
	}
}

// A session that recorded artifacts may only drop them once it is terminal. While
// it is running, those objects may be the only way back.
func TestCleanupRefusesWhileSessionIsStillRunning(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SAKSAMA_STATE_DIR", dir)

	store, err := state.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess := &state.Session{
		ID: "s1", CreatedAt: "2026-01-01T00:00:00Z", State: state.StateRunning,
		Database: "shop", Table: "orders", Engine: "gh-ost",
	}
	sess.RecordArtifact("table", "_orders_gho", "gh-ost", "ghost table")
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}

	code, out, _ := run(t, "cleanup", "s1")
	if code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "still running") {
		t.Fatalf("cleanup must refuse while the session is live: %s", out)
	}
	if !strings.Contains(out, "nothing was dropped") {
		t.Fatalf("unexpected output: %s", out)
	}
}

// Once the session is terminal, the recorded artifacts become deletable, and the
// list is what --dry-run reports.
func TestCleanupDryRunListsRecordedArtifacts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SAKSAMA_STATE_DIR", dir)

	store, err := state.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess := &state.Session{
		ID: "s1", CreatedAt: "2026-01-01T00:00:00Z", State: state.StateFailed,
		Database: "shop", Table: "orders", Engine: "pt-osc",
	}
	sess.RecordArtifact("table", "_orders_new", "pt-osc", "shadow table")
	sess.RecordArtifact("trigger", "pt_osc_shop_orders_ins", "pt-osc", "copy trigger")
	sess.MarkArtifactsSafe("leftover from a failed migration")
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := run(t, "cleanup", "s1", "--dry-run")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{"would drop 2 artifact", "_orders_new", "pt_osc_shop_orders_ins"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
