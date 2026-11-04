package introspect

import (
	"fmt"
	"strings"

	"github.com/perogeremmer/saksama-core/internal/model"
)

// artifactPatterns are the table names copy engines leave behind.
//
// gh-ost keeps the pre-migration table as _name_del after a successful cut-over,
// and pt-osc keeps _name_old. Both are real disk usage and both are the first
// thing to clean up, so they are surfaced rather than silently listed as tables.
var artifactPatterns = []struct {
	suffix  string
	owner   string
	meaning string
}{
	{"_gho", "gh-ost", "ghost table from an unfinished copy"},
	{"_ghc", "gh-ost", "gh-ost changelog table"},
	{"_ghk", "gh-ost", "gh-ost heartbeat table"},
	{"_del", "gh-ost", "old table kept after a successful cut-over"},
	{"_new", "pt-osc", "shadow table from an unfinished copy"},
	{"_old", "pt-osc", "old table kept after a successful swap"},
}

// triggerPatterns are the trigger names copy engines create.
var triggerPatterns = []struct{ prefix, owner string }{
	{"pt_osc_", "pt-osc"},
	{"ghost_", "gh-ost"},
	{"gst_", "gh-ost"},
}

// FindArtifacts reports leftover objects from previous migrations.
//
// It never guesses ownership from the name alone for deletion purposes; naming a
// leftover is useful, deleting by name is not (see the cleanup command).
func FindArtifacts(tables []model.Table) []Artifact {
	var out []Artifact

	for _, t := range tables {
		for _, p := range artifactPatterns {
			if !strings.HasSuffix(strings.ToLower(t.Name), p.suffix) {
				continue
			}
			origin := strings.TrimSuffix(t.Name, t.Name[len(t.Name)-len(p.suffix):])
			out = append(out, Artifact{
				Kind: "table",
				Name: t.Name,
				Evidence: fmt.Sprintf("%s artifact (from %s): %s, %s on disk",
					p.owner, strings.TrimPrefix(origin, "_"), p.meaning, human(t.SizeBytes())),
			})
			break
		}
		for _, trg := range t.Triggers {
			for _, p := range triggerPatterns {
				if strings.HasPrefix(strings.ToLower(trg), p.prefix) {
					out = append(out, Artifact{
						Kind:     "trigger",
						Name:     trg,
						Evidence: fmt.Sprintf("%s trigger left on %s", p.owner, t.Name),
					})
					break
				}
			}
		}
	}

	return out
}

// human renders a byte count compactly for the artifact line.
func human(n int64) string {
	if n <= 0 {
		return "unknown size"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}
