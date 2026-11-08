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

// Scope is how wide an engine choice reaches.
type Scope string

const (
	// ScopeGlobal applies to every table on every database.
	ScopeGlobal Scope = "global"
	// ScopeDatabase applies to every table in one database.
	ScopeDatabase Scope = "database"
	// ScopeTable applies to one table.
	ScopeTable Scope = "table"
)

// EngineChoice is a durable record of the operator's decision.
//
// The analyzer recommends; the human decides. Recording the decision (rather
// than re-deriving it every run) means `apply` executes what was agreed, and the
// audit trail can show that a person chose it on purpose.
type EngineChoice struct {
	Engine string `json:"engine"`
	Scope  Scope  `json:"scope"`
	// Database and Table are filled according to Scope.
	Database string `json:"database,omitempty"`
	Table    string `json:"table,omitempty"`
	Actor    string `json:"actor,omitempty"`
	Note     string `json:"note,omitempty"`
	SetAt    string `json:"set_at"`
}

// Label is the human-readable scope, e.g. "global", "shop.*", "shop.orders".
func (c EngineChoice) Label() string {
	switch c.Scope {
	case ScopeDatabase:
		return c.Database + ".*"
	case ScopeTable:
		return c.Database + "." + c.Table
	}
	return "global"
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

// NewChoice builds a choice for a scope, validating the arguments.
func NewChoice(engine string, global bool, database, table string) (EngineChoice, error) {
	e, err := NormalizeEngine(engine)
	if err != nil {
		return EngineChoice{}, err
	}

	c := EngineChoice{Engine: e, Database: strings.ToLower(strings.TrimSpace(database)),
		Table: strings.ToLower(strings.TrimSpace(table))}

	switch {
	case global:
		if c.Database != "" || c.Table != "" {
			return c, errors.New("--global cannot be combined with -d or -t")
		}
		c.Scope = ScopeGlobal
	case c.Database == "" && c.Table == "":
		// Typing just the engine means everything, the same as --global.
		c.Scope = ScopeGlobal
	case c.Database != "" && c.Table == "":
		c.Scope = ScopeDatabase
	case c.Database != "" && c.Table != "":
		c.Scope = ScopeTable
	default:
		return c, errors.New("-t requires -d")
	}

	return c, nil
}

func (s *Store) engineChoicePath(c EngineChoice) string {
	switch c.Scope {
	case ScopeDatabase:
		return filepath.Join(s.dir, "engine-db-"+safeKey(c.Database)+".json")
	case ScopeTable:
		return filepath.Join(s.dir, "engine-"+safeKey(c.Database)+"."+safeKey(c.Table)+".json")
	}
	return filepath.Join(s.dir, "engine-global.json")
}

// safeKey keeps filenames predictable while escaping anything exotic.
func safeKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		case r == '.':
			b.WriteString("_")
		default:
			b.WriteString("_")
		}
	}
	return b.String()
}

// SetEngineChoice persists the operator's choice.
func (s *Store) SetEngineChoice(c EngineChoice) error {
	engine, err := NormalizeEngine(c.Engine)
	if err != nil {
		return err
	}
	c.Engine = engine
	c.SetAt = time.Now().UTC().Format(time.RFC3339)

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	path := s.engineChoicePath(c)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	// The rename that publishes the choice is only durable once the directory
	// entry itself is flushed.
	return syncDir(s.dir)
}

func (s *Store) loadChoicePath(path string) (*EngineChoice, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var c EngineChoice
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("engine choice file %s is corrupt: %w", filepath.Base(path), err)
	}
	return &c, nil
}

// ResolveEngine picks the most specific recorded choice:
// table, then database, then global.
func (s *Store) ResolveEngine(database, table string) (*EngineChoice, error) {
	database = strings.ToLower(strings.TrimSpace(database))
	table = strings.ToLower(strings.TrimSpace(table))

	if database != "" && table != "" {
		if c, err := s.loadChoicePath(s.engineChoicePath(EngineChoice{Scope: ScopeTable, Database: database, Table: table})); err != nil || c != nil {
			return c, err
		}
	}
	if database != "" {
		if c, err := s.loadChoicePath(s.engineChoicePath(EngineChoice{Scope: ScopeDatabase, Database: database})); err != nil || c != nil {
			return c, err
		}
	}
	return s.loadChoicePath(s.engineChoicePath(EngineChoice{Scope: ScopeGlobal}))
}

// ClearEngineChoice removes a recorded choice and reports whether one was there.
//
// The boolean matters: clearing a table-scoped choice that was never recorded used to
// report success while a global choice carried on deciding the engine, so the operator
// was told a decision had been undone when nothing had changed.
func (s *Store) ClearEngineChoice(c EngineChoice) (bool, error) {
	err := os.Remove(s.engineChoicePath(c))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ListEngineChoices returns every recorded choice, most specific first.
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
	rank := map[Scope]int{ScopeTable: 0, ScopeDatabase: 1, ScopeGlobal: 2}
	sort.Slice(out, func(i, j int) bool {
		if rank[out[i].Scope] != rank[out[j].Scope] {
			return rank[out[i].Scope] < rank[out[j].Scope]
		}
		return out[i].Label() < out[j].Label()
	})
	return out, nil
}
