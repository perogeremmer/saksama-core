package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// EngineChoice is a durable record of the operator's decision.
//
// The analyzer recommends; the human decides. Recording the decision (rather
// than re-deriving it every run) means `apply` executes what was agreed, and the
// audit trail can show that a person chose it on purpose.
type EngineChoice struct {
	Engine string `json:"engine"`
	// Target is "database.table" when scoped to one table, or "" for the default.
	Target string `json:"target,omitempty"`
	Actor  string `json:"actor,omitempty"`
	Note   string `json:"note,omitempty"`
	SetAt  string `json:"set_at"`
}

// KnownEngines are the values the choice command accepts.
var KnownEngines = []string{"native", "gh-ost", "pt-osc"}

// NormalizeEngine maps user spellings onto the canonical names.
func NormalizeEngine(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "native", "ddl", "sql":
		return "native", nil
	case "gh-ost", "ghost", "gh":
		return "gh-ost", nil
	case "pt-osc", "ptosc", "pt", "percona", "percona-toolkit":
		return "pt-osc", nil
	}
	return "", fmt.Errorf("unknown engine %q (want one of: %s)", s, strings.Join(KnownEngines, ", "))
}

// NormalizeTarget canonicalises "db.table" (or empty for the default).
func NormalizeTarget(database, table string) string {
	if database == "" && table == "" {
		return ""
	}
	return strings.ToLower(database) + "." + strings.ToLower(table)
}

func (s *Store) engineChoicePath(target string) string {
	if target == "" {
		return filepath.Join(s.dir, "engine-default.json")
	}
	key := strings.NewReplacer("/", "_", "\\", "_", "..", "_").Replace(target)
	return filepath.Join(s.dir, "engine-"+key+".json")
}

// SetEngineChoice persists the operator's choice.
func (s *Store) SetEngineChoice(c EngineChoice) error {
	engine, err := NormalizeEngine(c.Engine)
	if err != nil {
		return err
	}
	c.Engine = engine
	c.Target = NormalizeTarget2(c.Target)
	c.SetAt = time.Now().UTC().Format(time.RFC3339)

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	path := s.engineChoicePath(c.Target)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// NormalizeTarget2 lower-cases an already-joined target string.
func NormalizeTarget2(target string) string { return strings.ToLower(strings.TrimSpace(target)) }

// EngineChoice returns the recorded choice for a target, or nil.
func (s *Store) EngineChoice(target string) (*EngineChoice, error) {
	data, err := os.ReadFile(s.engineChoicePath(NormalizeTarget2(target)))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var c EngineChoice
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("engine choice file is corrupt: %w", err)
	}
	return &c, nil
}

// ResolveEngine picks the effective engine: target choice, then the default.
func (s *Store) ResolveEngine(database, table string) (*EngineChoice, error) {
	if c, err := s.EngineChoice(NormalizeTarget(database, table)); err != nil || c != nil {
		return c, err
	}
	return s.EngineChoice("")
}

// ClearEngineChoice removes a recorded choice.
func (s *Store) ClearEngineChoice(target string) error {
	err := os.Remove(s.engineChoicePath(NormalizeTarget2(target)))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// ListEngineChoices returns every recorded choice, newest first.
func (s *Store) ListEngineChoices() ([]EngineChoice, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []EngineChoice
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "engine-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, name))
		if err != nil {
			continue
		}
		var c EngineChoice
		if json.Unmarshal(data, &c) == nil {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SetAt > out[j].SetAt })
	return out, nil
}
