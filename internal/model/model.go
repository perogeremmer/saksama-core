// Package model holds the neutral schema and ALTER representation shared by the
// parser, the analyzer, and the renderers. It has no database dependency.
package model

import (
	"fmt"
	"strings"
)

// Server describes the target MySQL server as far as the analyzer cares.
type Server struct {
	Host        string `json:"host"`
	Version     string `json:"version"`         // e.g. "8.0.36"
	VersionCmt  string `json:"version_comment"` // e.g. "MySQL Community Server - GPL"
	Vendor      string `json:"vendor"`          // mysql | percona | aurora | rds | mariadb | unknown
	DefaultChar string `json:"default_charset"`
	DefaultColl string `json:"default_collation"`
}

// Version is a parsed MySQL version.
type Version struct {
	Major, Minor, Patch int
	Raw                 string
}

// ParseVersion parses "8.0.36", "8.0.36-log", "5.7.44" or "10.6.16-MariaDB".
func ParseVersion(s string) (Version, error) {
	v := Version{Raw: s}
	core := s
	if i := strings.IndexAny(core, "-+"); i >= 0 {
		core = core[:i]
	}
	parts := strings.Split(core, ".")
	if len(parts) < 2 {
		return v, fmt.Errorf("cannot parse version %q", s)
	}
	if _, err := fmt.Sscanf(strings.Join(parts[:min(3, len(parts))], "."), "%d.%d.%d", &v.Major, &v.Minor, &v.Patch); err != nil {
		// Sscanf fails when only two components are present; retry with two.
		if _, e2 := fmt.Sscanf(strings.Join(parts[:2], "."), "%d.%d", &v.Major, &v.Minor); e2 != nil {
			return v, fmt.Errorf("cannot parse version %q: %w", s, err)
		}
		v.Patch = 0
	}
	return v, nil
}

// AtLeast reports whether v >= (major, minor, patch).
func (v Version) AtLeast(major, minor, patch int) bool {
	if v.Major != major {
		return v.Major > major
	}
	if v.Minor != minor {
		return v.Minor > minor
	}
	return v.Patch >= patch
}

func (v Version) String() string {
	if v.Raw != "" {
		return v.Raw
	}
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// IsMariaDB reports whether the server is MariaDB.
func (s Server) IsMariaDB() bool {
	l := strings.ToLower(s.VersionCmt + " " + s.Vendor + " " + s.Version)
	return strings.Contains(l, "mariadb")
}

// IsManaged reports whether the server looks like a managed cloud service.
func (s Server) IsManaged() bool {
	switch strings.ToLower(s.Vendor) {
	case "aurora", "rds":
		return true
	}
	l := strings.ToLower(s.VersionCmt)
	return strings.Contains(l, "amazon") || strings.Contains(l, "aurora") || strings.Contains(l, "cloud sql")
}

// Column is one table column.
type Column struct {
	Name      string  `json:"name"`
	Type      string  `json:"type"`      // normalized, lower-case, e.g. "varchar(191)"
	DataType  string  `json:"data_type"` // base type without length, e.g. "varchar"
	Charset   string  `json:"charset"`
	Collation string  `json:"collation"`
	Nullable  bool    `json:"nullable"`
	Default   *string `json:"default"`
	Extra     string  `json:"extra"` // auto_increment, GENERATED, DEFAULT_GENERATED
	Generated bool    `json:"generated"`
	OnUpdate  string  `json:"on_update"`
}

// IsTextual reports whether the type is a character type.
func (c Column) IsTextual() bool {
	switch strings.ToLower(c.DataType) {
	case "char", "varchar", "tinytext", "text", "mediumtext", "longtext", "enum", "set":
		return true
	}
	return false
}

// IsBlobLike reports whether the column requires a prefix length in an index.
func (c Column) IsBlobLike() bool {
	switch strings.ToLower(c.DataType) {
	case "tinytext", "text", "mediumtext", "longtext", "blob", "tinyblob", "mediumblob", "longblob", "json", "geometry", "point", "linestring", "polygon":
		return true
	}
	return false
}

// IsUUIDLike reports whether the column looks like a UUID primary key.
func (c Column) IsUUIDLike() bool {
	t := strings.ToLower(c.DataType)
	if t == "uuid" {
		return true
	}
	if (t == "char" || t == "varchar" || t == "binary" || t == "varbinary") && strings.Contains(c.Type, "(36)") {
		return true
	}
	return false
}

// IndexPart is one column of an index, optionally with a prefix length.
type IndexPart struct {
	Column    string `json:"column"`
	PrefixLen int    `json:"prefix_len,omitempty"` // 0 = full column
	Desc      bool   `json:"desc,omitempty"`
}

// Index is one index definition.
type Index struct {
	Name     string      `json:"name"`
	Unique   bool        `json:"unique"`
	Primary  bool        `json:"primary"`
	Fulltext bool        `json:"fulltext,omitempty"`
	Spatial  bool        `json:"spatial,omitempty"`
	Parts    []IndexPart `json:"parts"`
}

// FKAction is a referential action.
type FKAction string

const (
	ActionNoAction   FKAction = "NO ACTION"
	ActionRestrict   FKAction = "RESTRICT"
	ActionCascade    FKAction = "CASCADE"
	ActionSetNull    FKAction = "SET NULL"
	ActionSetDefault FKAction = "SET DEFAULT"
)

// ForeignKey is one FK constraint, including its referential actions.
// Actions are mandatory: losing them silently changes data semantics.
type ForeignKey struct {
	Name       string   `json:"name"`
	Columns    []string `json:"columns"`
	RefSchema  string   `json:"ref_schema"`
	RefTable   string   `json:"ref_table"`
	RefColumns []string `json:"ref_columns"`
	OnDelete   FKAction `json:"on_delete"`
	OnUpdate   FKAction `json:"on_update"`
	Match      string   `json:"match,omitempty"`
}

// HasAction reports whether the FK carries a non-default referential action.
func (fk ForeignKey) HasAction() bool {
	return (fk.OnDelete != "" && fk.OnDelete != ActionNoAction && fk.OnDelete != ActionRestrict) ||
		(fk.OnUpdate != "" && fk.OnUpdate != ActionNoAction && fk.OnUpdate != ActionRestrict)
}

// Table is the introspected table model.
type Table struct {
	Schema       string       `json:"schema"`
	Name         string       `json:"name"`
	Engine       string       `json:"engine"`
	RowFormat    string       `json:"row_format"`
	Charset      string       `json:"charset"`
	Collation    string       `json:"collation"`
	Rows         int64        `json:"rows"`         // estimated
	DataLength   int64        `json:"data_length"`  // bytes
	IndexLength  int64        `json:"index_length"` // bytes
	Partitioned  bool         `json:"partitioned"`
	Columns      []Column     `json:"columns"`
	Indexes      []Index      `json:"indexes"`
	ForeignKeys  []ForeignKey `json:"foreign_keys"`  // outgoing FKs on this table
	ReferencedBy []ForeignKey `json:"referenced_by"` // incoming FKs from other tables
	Triggers     []string     `json:"triggers"`
}

// Column returns the named column.
func (t Table) Column(name string) (Column, bool) {
	for _, c := range t.Columns {
		if strings.EqualFold(c.Name, name) {
			return c, true
		}
	}
	return Column{}, false
}

// PrimaryKey returns the primary index, if any.
func (t Table) PrimaryKey() (Index, bool) {
	for _, ix := range t.Indexes {
		if ix.Primary {
			return ix, true
		}
	}
	return Index{}, false
}

// HasUniqueKey reports whether the table has a primary or unique index.
func (t Table) HasUniqueKey() bool {
	for _, ix := range t.Indexes {
		if ix.Primary || ix.Unique {
			return true
		}
	}
	return false
}

// TriggersAny reports whether the table already has triggers of its own.
func (t Table) TriggersAny() bool { return len(t.Triggers) > 0 }

// AllForeignKeys returns outgoing and incoming FKs.
func (t Table) AllForeignKeys() []ForeignKey {
	out := make([]ForeignKey, 0, len(t.ForeignKeys)+len(t.ReferencedBy))
	out = append(out, t.ForeignKeys...)
	out = append(out, t.ReferencedBy...)
	return out
}

// SizeBytes is the total table size on disk.
func (t Table) SizeBytes() int64 { return t.DataLength + t.IndexLength }

// OpKind enumerates the ALTER operations Saksama understands.
type OpKind string

const (
	OpAddIndex       OpKind = "add_index"
	OpAddUniqueIndex OpKind = "add_unique_index"
	OpAddFulltext    OpKind = "add_fulltext_index"
	OpAddSpatial     OpKind = "add_spatial_index"
	OpDropIndex      OpKind = "drop_index"
	OpAddColumn      OpKind = "add_column"
	OpDropColumn     OpKind = "drop_column"
	OpModifyColumn   OpKind = "modify_column"
	OpChangeColumn   OpKind = "change_column"
	OpRenameColumn   OpKind = "rename_column"
	OpAddPrimaryKey  OpKind = "add_primary_key"
	OpDropPrimaryKey OpKind = "drop_primary_key"
	OpAddForeignKey  OpKind = "add_foreign_key"
	OpDropForeignKey OpKind = "drop_foreign_key"
	OpRenameTable    OpKind = "rename_table"
	OpConvertCharset OpKind = "convert_charset"
	OpEngineChange   OpKind = "engine_change"
	OpSetDefault     OpKind = "set_default"
	OpDropDefault    OpKind = "drop_default"
	OpUnknown        OpKind = "unknown"
)

// ColumnPosition describes FIRST / AFTER col placement for ADD COLUMN.
type ColumnPosition struct {
	First bool
	After string
	Set   bool
}

// Op is one parsed ALTER operation.
type Op struct {
	Kind OpKind `json:"kind"`
	Raw  string `json:"raw"`

	// Index operations.
	IndexName string      `json:"index_name,omitempty"`
	Parts     []IndexPart `json:"parts,omitempty"`

	// Column operations.
	Column   *Column         `json:"column,omitempty"`
	OldName  string          `json:"old_name,omitempty"`
	Position *ColumnPosition `json:"position,omitempty"`

	// Foreign key operations.
	ForeignKey *ForeignKey `json:"foreign_key,omitempty"`

	// Table-level operations.
	NewName      string  `json:"new_name,omitempty"`
	Charset      string  `json:"charset,omitempty"`
	Collation    string  `json:"collation,omitempty"`
	Engine       string  `json:"engine,omitempty"`
	DefaultValue *string `json:"default_value,omitempty"`
}

// RefColumns returns the column names an operation touches, when determinable.
func (o Op) RefColumns() []string {
	var out []string
	if o.Column != nil && o.Column.Name != "" {
		out = append(out, o.Column.Name)
	}
	if o.OldName != "" {
		out = append(out, o.OldName)
	}
	for _, p := range o.Parts {
		if p.Column != "" {
			out = append(out, p.Column)
		}
	}
	return out
}
