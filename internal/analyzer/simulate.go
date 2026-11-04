package analyzer

import (
	"fmt"
	"strings"

	"github.com/perogeremmer/saksama-core/internal/model"
)

// applyOps projects the post-ALTER table so checks can reason about the schema
// the migration will actually produce, not just the schema it starts from.
//
// It returns notes for operations it cannot simulate. Those notes are surfaced
// in the plan rather than silently ignored.
func applyOps(tbl model.Table, ops []model.Op) (model.Table, []string) {
	out := cloneTable(tbl)
	var notes []string

	for _, op := range ops {
		switch op.Kind {
		case model.OpAddIndex, model.OpAddUniqueIndex, model.OpAddFulltext, model.OpAddSpatial:
			name := op.IndexName
			if name == "" && len(op.Parts) > 0 {
				name = op.Parts[0].Column
			}
			ix := model.Index{
				Name:     name,
				Parts:    op.Parts,
				Unique:   op.Kind == model.OpAddUniqueIndex,
				Fulltext: op.Kind == model.OpAddFulltext,
				Spatial:  op.Kind == model.OpAddSpatial,
			}
			replaced := false
			for i := range out.Indexes {
				if strings.EqualFold(out.Indexes[i].Name, name) {
					out.Indexes[i] = ix
					replaced = true
					break
				}
			}
			if !replaced {
				out.Indexes = append(out.Indexes, ix)
			}

		case model.OpDropIndex:
			for i := range out.Indexes {
				if strings.EqualFold(out.Indexes[i].Name, op.IndexName) && !out.Indexes[i].Primary {
					out.Indexes = append(out.Indexes[:i], out.Indexes[i+1:]...)
					break
				}
			}

		case model.OpAddColumn:
			if op.Column == nil {
				notes = append(notes, "cannot simulate ADD COLUMN without a column definition")
				continue
			}
			col := *op.Column
			if col.Charset == "" && col.IsTextual() {
				col.Charset = out.Charset
			}
			if col.Collation == "" && col.IsTextual() {
				col.Collation = out.Collation
			}
			replaced := false
			for i := range out.Columns {
				if strings.EqualFold(out.Columns[i].Name, col.Name) {
					out.Columns[i] = col
					replaced = true
					break
				}
			}
			if !replaced {
				out.Columns = append(out.Columns, col)
			}

		case model.OpDropColumn:
			if op.Column == nil {
				continue
			}
			for i := range out.Columns {
				if strings.EqualFold(out.Columns[i].Name, op.Column.Name) {
					out.Columns = append(out.Columns[:i], out.Columns[i+1:]...)
					break
				}
			}
			dropIndexesOnColumn(&out, op.Column.Name)

		case model.OpModifyColumn:
			if op.Column == nil {
				continue
			}
			for i := range out.Columns {
				if strings.EqualFold(out.Columns[i].Name, op.Column.Name) {
					col := *op.Column
					if col.Charset == "" && col.IsTextual() {
						col.Charset = out.Charset
					}
					out.Columns[i] = col
					break
				}
			}

		case model.OpChangeColumn:
			if op.Column == nil {
				continue
			}
			for i := range out.Columns {
				if strings.EqualFold(out.Columns[i].Name, op.OldName) {
					col := *op.Column
					if col.Charset == "" && col.IsTextual() {
						col.Charset = out.Charset
					}
					out.Columns[i] = col
					renameInIndexes(&out, op.OldName, col.Name)
					break
				}
			}

		case model.OpRenameColumn:
			for i := range out.Columns {
				if strings.EqualFold(out.Columns[i].Name, op.OldName) {
					out.Columns[i].Name = op.NewName
					renameInIndexes(&out, op.OldName, op.NewName)
					break
				}
			}

		case model.OpAddPrimaryKey:
			for i := range out.Indexes {
				out.Indexes[i].Primary = false
			}
			out.Indexes = append(out.Indexes, model.Index{Name: "PRIMARY", Unique: true, Primary: true, Parts: op.Parts})

		case model.OpDropPrimaryKey:
			for i := range out.Indexes {
				out.Indexes[i].Primary = false
			}

		case model.OpSetDefault, model.OpDropDefault:
			if op.Column == nil {
				continue
			}
			for i := range out.Columns {
				if strings.EqualFold(out.Columns[i].Name, op.Column.Name) {
					out.Columns[i].Default = op.DefaultValue
					break
				}
			}

		case model.OpConvertCharset:
			out.Charset = strings.ToLower(op.Charset)
			if op.Collation != "" {
				out.Collation = strings.ToLower(op.Collation)
			}
			for i := range out.Columns {
				if out.Columns[i].IsTextual() {
					out.Columns[i].Charset = out.Charset
					out.Columns[i].Collation = out.Collation
				}
			}

		case model.OpRenameTable:
			out.Name = op.NewName

		case model.OpEngineChange:
			out.Engine = op.Engine

		case model.OpAddForeignKey, model.OpDropForeignKey:
			notes = append(notes, "foreign key DDL is out of scope in v1; the projected schema does not include it")

		default:
			notes = append(notes, fmt.Sprintf("cannot simulate operation %q (%s)", op.Raw, op.Kind))
		}
	}

	return out, notes
}

func cloneTable(t model.Table) model.Table {
	out := t
	out.Columns = append([]model.Column(nil), t.Columns...)
	out.ForeignKeys = append([]model.ForeignKey(nil), t.ForeignKeys...)
	out.ReferencedBy = append([]model.ForeignKey(nil), t.ReferencedBy...)
	out.Triggers = append([]string(nil), t.Triggers...)
	out.Indexes = make([]model.Index, len(t.Indexes))
	for i, ix := range t.Indexes {
		ni := ix
		ni.Parts = append([]model.IndexPart(nil), ix.Parts...)
		out.Indexes[i] = ni
	}
	return out
}

func renameInIndexes(t *model.Table, oldName, newName string) {
	for i := range t.Indexes {
		for j := range t.Indexes[i].Parts {
			if strings.EqualFold(t.Indexes[i].Parts[j].Column, oldName) {
				t.Indexes[i].Parts[j].Column = newName
			}
		}
	}
	for i := range t.ForeignKeys {
		for j := range t.ForeignKeys[i].Columns {
			if strings.EqualFold(t.ForeignKeys[i].Columns[j], oldName) {
				t.ForeignKeys[i].Columns[j] = newName
			}
		}
	}
}

func dropIndexesOnColumn(t *model.Table, column string) {
	kept := t.Indexes[:0]
	for _, ix := range t.Indexes {
		uses := false
		for _, p := range ix.Parts {
			if strings.EqualFold(p.Column, column) {
				uses = true
				break
			}
		}
		if !uses {
			kept = append(kept, ix)
		}
	}
	t.Indexes = kept
}
