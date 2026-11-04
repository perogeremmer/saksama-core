package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The compatibility document is generated, so it must not drift from the data in
// internal/support. This test is the guard: if it fails, run `make compat-doc`.
func TestCompatibilityDocIsUpToDate(t *testing.T) {
	var buf bytes.Buffer
	code := Run([]string{"compat", "--markdown"}, &buf, &buf)
	if code != ExitOK {
		t.Fatalf("generate: exit %d", code)
	}

	const path = "../../docs/COMPATIBILITY.md"
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	generated := strings.TrimSpace(buf.String())
	committed := strings.TrimSpace(string(onDisk))
	if generated == committed {
		return
	}

	// Report the first differing line, which is far easier to read than a diff.
	g := strings.Split(generated, "\n")
	c := strings.Split(committed, "\n")
	for i := 0; i < len(g) || i < len(c); i++ {
		var gl, cl string
		if i < len(g) {
			gl = g[i]
		}
		if i < len(c) {
			cl = c[i]
		}
		if gl != cl {
			t.Fatalf("docs/COMPATIBILITY.md is stale: run `make compat-doc`.\nline %d:\n  generated: %q\n  committed: %q",
				i+1, gl, cl)
		}
	}
	t.Fatal("docs/COMPATIBILITY.md is stale: run `make compat-doc`")
}

func TestCompatJSONDescribesEverySection(t *testing.T) {
	code, out, errOut := run(t, "compat", "--json", "--no-probe")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	for _, key := range []string{"mysql", "engines", "findings", "postgres"} {
		if _, ok := payload[key]; !ok {
			t.Errorf("missing %q", key)
		}
	}
	engines := payload["engines"].([]any)
	if len(engines) == 0 {
		t.Fatal("no engine rows")
	}
	// Every row must state its evidence level; that is the point of the document.
	for _, raw := range engines {
		row := raw.(map[string]any)
		if row["status"] == "" {
			t.Errorf("engine row without a status: %v", row)
		}
	}
}

func TestCompatLocalReportIsReadable(t *testing.T) {
	code, out, _ := run(t, "compat", "--no-probe")
	if code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "LOCAL COMPATIBILITY") {
		t.Fatalf("unexpected output:\n%s", out)
	}
	if !strings.Contains(out, "docs/COMPATIBILITY.md") {
		t.Fatalf("must point at the full matrix:\n%s", out)
	}
	for _, r := range out {
		if r > 127 {
			t.Fatalf("non-ASCII rune %q in compat output", r)
		}
	}
}
