package parser

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/perogeremmer/saksama-core/internal/model"
)

// Alter is the parsed form of an ALTER TABLE statement.
type Alter struct {
	Schema    string
	Table     string
	Ops       []model.Op
	Algorithm string // explicit ALGORITHM= value, if any
	Lock      string // explicit LOCK= value, if any
	Raw       string
}

// ParseAlter parses a single ALTER TABLE statement.
//
// The parser is intentionally narrow: anything it does not fully understand
// becomes model.OpUnknown, which the analyzer turns into a blocker. It never
// guesses and never silently downgrades.
func ParseAlter(sql string) (Alter, error) {
	tokens, err := Tokenize(sql)
	if err != nil {
		return Alter{}, err
	}
	if err := RejectMultiStatement(tokens); err != nil {
		return Alter{}, err
	}
	tokens = TrimSemicolons(tokens)

	a := Alter{Raw: strings.TrimSpace(sql)}

	i := 0
	if i >= len(tokens) || !tokens[i].IsWord("ALTER") {
		return a, fmt.Errorf("statement must start with ALTER")
	}
	i++
	if i >= len(tokens) || !tokens[i].IsWord("TABLE") {
		return a, fmt.Errorf("expected TABLE after ALTER")
	}
	i++

	// Optional IF EXISTS / IF NOT EXISTS.
	if i+1 < len(tokens) && tokens[i].IsWord("IF") && tokens[i+1].IsWord("EXISTS") {
		i += 2
	}

	// Qualified table name.
	if i >= len(tokens) || !tokens[i].IsIdentifier() {
		return a, fmt.Errorf("expected table name")
	}
	a.Table = tokens[i].Value
	i++
	if i < len(tokens) && tokens[i].IsPunct(".") {
		if i+1 >= len(tokens) || !tokens[i+1].IsIdentifier() {
			return a, fmt.Errorf("expected table name after qualifier")
		}
		a.Schema = a.Table
		a.Table = tokens[i+1].Value
		i += 2
	}

	rest := tokens[i:]
	if len(rest) == 0 {
		return a, fmt.Errorf("ALTER TABLE has no operations")
	}

	parts := SplitTopLevel(rest)
	for _, part := range parts {
		if len(part) == 0 {
			return a, fmt.Errorf("empty operation between commas")
		}

		// Statement-level options may appear alone or after operations.
		if len(part) >= 2 && part[0].IsWord("ALGORITHM") {
			a.Algorithm = strings.ToUpper(optionValue(part[1:]))
			continue
		}
		if len(part) >= 2 && part[0].IsWord("LOCK") {
			a.Lock = strings.ToUpper(optionValue(part[1:]))
			continue
		}

		op, err := parseOp(part)
		if err != nil {
			// Fail closed: unknown shape becomes an explicit unknown op.
			op = model.Op{Kind: model.OpUnknown, Raw: JoinTokens(part)}
		}
		a.Ops = append(a.Ops, op)
	}

	if len(a.Ops) == 0 {
		return a, fmt.Errorf("ALTER TABLE has no recognised operations")
	}

	return a, nil
}

func optionValue(tokens []Token) string {
	tokens = trimLeadingEquals(tokens)
	if len(tokens) == 0 {
		return ""
	}
	return tokens[0].Value
}

func trimLeadingEquals(tokens []Token) []Token {
	if len(tokens) > 0 && tokens[0].IsPunct("=") {
		return tokens[1:]
	}
	return tokens
}

// parseOp parses one top-level ALTER operation.
func parseOp(part []Token) (model.Op, error) {
	raw := JoinTokens(part)
	first := part[0]

	switch {
	case first.IsWord("ADD"):
		op, err := parseAdd(part[1:])
		op.Raw = raw
		return op, err

	case first.IsWord("DROP"):
		op, err := parseDrop(part[1:])
		op.Raw = raw
		return op, err

	case first.IsWord("MODIFY"):
		rest := part[1:]
		if len(rest) > 0 && rest[0].IsWord("COLUMN") {
			rest = rest[1:]
		}
		col, pos, err := parseColumnDef(rest)
		if err != nil {
			return model.Op{}, err
		}
		return model.Op{Kind: model.OpModifyColumn, Raw: raw, Column: &col, Position: pos}, nil

	case first.IsWord("CHANGE"):
		rest := part[1:]
		if len(rest) > 0 && rest[0].IsWord("COLUMN") {
			rest = rest[1:]
		}
		if len(rest) < 2 || !rest[0].IsIdentifier() {
			return model.Op{}, fmt.Errorf("CHANGE requires old and new column name")
		}
		oldName := rest[0].Value
		col, pos, err := parseColumnDef(rest[1:])
		if err != nil {
			return model.Op{}, err
		}
		return model.Op{Kind: model.OpChangeColumn, Raw: raw, OldName: oldName, Column: &col, Position: pos}, nil

	case first.IsWord("RENAME"):
		return parseRename(part[1:], raw)

	case first.IsWord("CONVERT"):
		// CONVERT TO CHARACTER SET cs [COLLATE coll]
		rest := part[1:]
		if len(rest) < 4 || !rest[0].IsWord("TO") || !rest[1].IsWord("CHARACTER") || !rest[2].IsWord("SET") {
			return model.Op{}, fmt.Errorf("unsupported CONVERT form")
		}
		op := model.Op{Kind: model.OpConvertCharset, Raw: raw, Charset: rest[3].Value}
		if len(rest) >= 6 && rest[4].IsWord("COLLATE") {
			op.Collation = rest[5].Value
		}
		return op, nil

	case first.IsWord("DEFAULT"):
		// DEFAULT CHARACTER SET cs [COLLATE coll]
		rest := part[1:]
		if len(rest) < 3 || !rest[0].IsWord("CHARACTER") || !rest[1].IsWord("SET") {
			return model.Op{}, fmt.Errorf("unsupported DEFAULT form")
		}
		op := model.Op{Kind: model.OpConvertCharset, Raw: raw, Charset: rest[2].Value}
		if len(rest) >= 5 && rest[3].IsWord("COLLATE") {
			op.Collation = rest[4].Value
		}
		return op, nil

	case first.IsWord("ENGINE"):
		rest := trimLeadingEquals(part[1:])
		if len(rest) == 0 {
			return model.Op{}, fmt.Errorf("ENGINE requires a value")
		}
		return model.Op{Kind: model.OpEngineChange, Raw: raw, Engine: rest[0].Value}, nil

	case first.IsWord("ALTER"):
		return parseAlterColumn(part[1:], raw)
	}

	return model.Op{Kind: model.OpUnknown, Raw: raw}, fmt.Errorf("unsupported operation %q", first.Value)
}

func parseAdd(rest []Token) (model.Op, error) {
	if len(rest) == 0 {
		return model.Op{}, fmt.Errorf("ADD requires an argument")
	}

	// ADD [COLUMN] <coldef>
	if rest[0].IsWord("COLUMN") {
		col, pos, err := parseColumnDef(rest[1:])
		if err != nil {
			return model.Op{}, err
		}
		return model.Op{Kind: model.OpAddColumn, Column: &col, Position: pos}, nil
	}

	switch {
	case rest[0].IsWord("INDEX") || rest[0].IsWord("KEY"):
		return parseAddIndex(rest[1:], model.OpAddIndex)
	case rest[0].IsWord("UNIQUE"):
		tail := rest[1:]
		if len(tail) > 0 && (tail[0].IsWord("INDEX") || tail[0].IsWord("KEY")) {
			tail = tail[1:]
		}
		return parseAddIndex(tail, model.OpAddUniqueIndex)
	case rest[0].IsWord("FULLTEXT"):
		tail := rest[1:]
		if len(tail) > 0 && (tail[0].IsWord("INDEX") || tail[0].IsWord("KEY")) {
			tail = tail[1:]
		}
		return parseAddIndex(tail, model.OpAddFulltext)
	case rest[0].IsWord("SPATIAL"):
		tail := rest[1:]
		if len(tail) > 0 && (tail[0].IsWord("INDEX") || tail[0].IsWord("KEY")) {
			tail = tail[1:]
		}
		return parseAddIndex(tail, model.OpAddSpatial)
	case rest[0].IsWord("PRIMARY"):
		tail := rest[1:]
		if len(tail) > 0 && tail[0].IsWord("KEY") {
			tail = tail[1:]
		}
		parts, _, err := parseIndexParts(tail)
		if err != nil {
			return model.Op{}, err
		}
		return model.Op{Kind: model.OpAddPrimaryKey, IndexName: "PRIMARY", Parts: parts}, nil
	case rest[0].IsWord("CONSTRAINT"):
		name := ""
		tail := rest[1:]
		if len(tail) > 0 && tail[0].IsIdentifier() && !tail[0].IsWord("FOREIGN") && !tail[0].IsWord("UNIQUE") && !tail[0].IsWord("PRIMARY") && !tail[0].IsWord("CHECK") {
			name = tail[0].Value
			tail = tail[1:]
		}
		if len(tail) > 0 && tail[0].IsWord("FOREIGN") {
			fk, err := parseForeignKey(name, tail)
			if err != nil {
				return model.Op{}, err
			}
			return model.Op{Kind: model.OpAddForeignKey, ForeignKey: fk}, nil
		}
		return model.Op{}, fmt.Errorf("unsupported constraint form")
	case rest[0].IsWord("FOREIGN"):
		fk, err := parseForeignKey("", rest)
		if err != nil {
			return model.Op{}, err
		}
		return model.Op{Kind: model.OpAddForeignKey, ForeignKey: fk}, nil
	}

	// Bare ADD <coldef>.
	col, pos, err := parseColumnDef(rest)
	if err != nil {
		return model.Op{}, err
	}
	return model.Op{Kind: model.OpAddColumn, Column: &col, Position: pos}, nil
}

func parseAddIndex(rest []Token, kind model.OpKind) (model.Op, error) {
	name := ""
	// Optional index name before the column list.
	if len(rest) > 0 && rest[0].IsIdentifier() && !rest[0].IsWord("USING") {
		name = rest[0].Value
		rest = rest[1:]
	}
	// Optional USING BTREE|HASH.
	if len(rest) > 0 && rest[0].IsWord("USING") {
		rest = rest[2:]
	}
	parts, _, err := parseIndexParts(rest)
	if err != nil {
		return model.Op{}, err
	}
	if len(parts) == 0 {
		return model.Op{}, fmt.Errorf("index requires at least one column")
	}
	return model.Op{Kind: kind, IndexName: name, Parts: parts}, nil
}

func parseIndexParts(tokens []Token) ([]model.IndexPart, []Token, error) {
	// Skip USing/type prefixes.
	for len(tokens) > 0 && tokens[0].IsWord("USING") {
		tokens = tokens[2:]
	}
	if len(tokens) == 0 || !tokens[0].IsPunct("(") {
		return nil, tokens, fmt.Errorf("expected '(' for index columns")
	}
	inner, next, err := consumeParen(tokens, 0)
	if err != nil {
		return nil, tokens, err
	}
	var parts []model.IndexPart
	for _, chunk := range SplitTopLevel(inner) {
		if len(chunk) == 0 {
			continue
		}
		p := model.IndexPart{}
		if !chunk[0].IsIdentifier() {
			return nil, tokens, fmt.Errorf("index column must be an identifier")
		}
		p.Column = chunk[0].Value
		rest := chunk[1:]
		if len(rest) > 0 && rest[0].IsPunct("(") {
			sub, after, err := consumeParen(rest, 0)
			if err != nil {
				return nil, tokens, err
			}
			if len(sub) == 1 && sub[0].Kind == KindNumber {
				n, err := strconv.Atoi(sub[0].Value)
				if err != nil {
					return nil, tokens, fmt.Errorf("invalid prefix length %q", sub[0].Value)
				}
				p.PrefixLen = n
			}
			rest = rest[after:]
		}
		if len(rest) > 0 && (rest[0].IsWord("DESC") || rest[0].IsWord("ASC")) {
			p.Desc = rest[0].IsWord("DESC")
		}
		parts = append(parts, p)
	}
	return parts, tokens[next:], nil
}

func parseForeignKey(name string, tokens []Token) (*model.ForeignKey, error) {
	// FOREIGN KEY [index_name] (cols) REFERENCES tbl (cols) [MATCH ...] [ON DELETE ...] [ON UPDATE ...]
	i := 0
	if i >= len(tokens) || !tokens[i].IsWord("FOREIGN") {
		return nil, fmt.Errorf("expected FOREIGN")
	}
	i++
	if i >= len(tokens) || !tokens[i].IsWord("KEY") {
		return nil, fmt.Errorf("expected KEY")
	}
	i++
	if i < len(tokens) && tokens[i].IsIdentifier() && !tokens[i].IsPunct("(") {
		if name == "" {
			name = tokens[i].Value
		}
		i++
	}
	if i >= len(tokens) || !tokens[i].IsPunct("(") {
		return nil, fmt.Errorf("expected column list in FOREIGN KEY")
	}
	inner, next, err := consumeParen(tokens, i)
	if err != nil {
		return nil, err
	}
	fk := &model.ForeignKey{Name: name, OnDelete: model.ActionRestrict, OnUpdate: model.ActionRestrict}
	for _, chunk := range SplitTopLevel(inner) {
		if len(chunk) > 0 && chunk[0].IsIdentifier() {
			fk.Columns = append(fk.Columns, chunk[0].Value)
		}
	}
	i = next

	if i >= len(tokens) || !tokens[i].IsWord("REFERENCES") {
		return nil, fmt.Errorf("expected REFERENCES")
	}
	i++
	if i >= len(tokens) || !tokens[i].IsIdentifier() {
		return nil, fmt.Errorf("expected referenced table")
	}
	fk.RefTable = tokens[i].Value
	i++
	if i < len(tokens) && tokens[i].IsPunct(".") {
		fk.RefSchema = fk.RefTable
		if i+1 >= len(tokens) || !tokens[i+1].IsIdentifier() {
			return nil, fmt.Errorf("expected referenced table after qualifier")
		}
		fk.RefTable = tokens[i+1].Value
		i += 2
	}
	if i >= len(tokens) || !tokens[i].IsPunct("(") {
		return nil, fmt.Errorf("expected referenced column list")
	}
	inner, next, err = consumeParen(tokens, i)
	if err != nil {
		return nil, err
	}
	for _, chunk := range SplitTopLevel(inner) {
		if len(chunk) > 0 && chunk[0].IsIdentifier() {
			fk.RefColumns = append(fk.RefColumns, chunk[0].Value)
		}
	}
	i = next

	for i < len(tokens) {
		switch {
		case tokens[i].IsWord("MATCH"):
			if i+1 < len(tokens) {
				fk.Match = strings.ToUpper(tokens[i+1].Value)
			}
			i += 2
		case tokens[i].IsWord("ON"):
			if i+2 >= len(tokens) {
				return nil, fmt.Errorf("malformed ON clause")
			}
			which := tokens[i+1]
			action, consumed, err := parseFKAction(tokens[i+2:])
			if err != nil {
				return nil, err
			}
			switch {
			case which.IsWord("DELETE") && which.Kind == KindWord:
				fk.OnDelete = action
			case which.IsWord("UPDATE"):
				fk.OnUpdate = action
			default:
				return nil, fmt.Errorf("unsupported ON clause %q", which.Value)
			}
			i += 2 + consumed
		default:
			i++
		}
	}

	return fk, nil
}

func parseFKAction(tokens []Token) (model.FKAction, int, error) {
	if len(tokens) == 0 {
		return "", 0, fmt.Errorf("missing referential action")
	}
	switch {
	case tokens[0].IsWord("CASCADE"):
		return model.ActionCascade, 1, nil
	case tokens[0].IsWord("RESTRICT"):
		return model.ActionRestrict, 1, nil
	case tokens[0].IsWord("NO") && len(tokens) > 1 && tokens[1].IsWord("ACTION"):
		return model.ActionNoAction, 2, nil
	case tokens[0].IsWord("SET") && len(tokens) > 1 && tokens[1].IsWord("NULL"):
		return model.ActionSetNull, 2, nil
	case tokens[0].IsWord("SET") && len(tokens) > 1 && tokens[1].IsWord("DEFAULT"):
		return model.ActionSetDefault, 2, nil
	}
	return "", 0, fmt.Errorf("unsupported referential action %q", tokens[0].Value)
}

func parseDrop(rest []Token) (model.Op, error) {
	if len(rest) == 0 {
		return model.Op{}, fmt.Errorf("DROP requires an argument")
	}
	switch {
	case rest[0].IsWord("INDEX") || rest[0].IsWord("KEY"):
		if len(rest) < 2 || !rest[1].IsIdentifier() {
			return model.Op{}, fmt.Errorf("DROP INDEX requires a name")
		}
		return model.Op{Kind: model.OpDropIndex, IndexName: rest[1].Value}, nil
	case rest[0].IsWord("PRIMARY"):
		return model.Op{Kind: model.OpDropPrimaryKey, IndexName: "PRIMARY"}, nil
	case rest[0].IsWord("FOREIGN"):
		if len(rest) < 3 || !rest[2].IsIdentifier() {
			return model.Op{}, fmt.Errorf("DROP FOREIGN KEY requires a name")
		}
		return model.Op{Kind: model.OpDropForeignKey, ForeignKey: &model.ForeignKey{Name: rest[2].Value}}, nil
	case rest[0].IsWord("COLUMN"):
		if len(rest) < 2 || !rest[1].IsIdentifier() {
			return model.Op{}, fmt.Errorf("DROP COLUMN requires a name")
		}
		return model.Op{Kind: model.OpDropColumn, Column: &model.Column{Name: rest[1].Value}}, nil
	case rest[0].IsWord("CHECK") || rest[0].IsWord("CONSTRAINT") || rest[0].IsWord("PARTITION"):
		return model.Op{Kind: model.OpUnknown, Raw: JoinTokens(rest)}, fmt.Errorf("unsupported DROP %s", rest[0].Value)
	}
	if !rest[0].IsIdentifier() {
		return model.Op{}, fmt.Errorf("DROP expects an identifier")
	}
	return model.Op{Kind: model.OpDropColumn, Column: &model.Column{Name: rest[0].Value}}, nil
}

func parseRename(rest []Token, raw string) (model.Op, error) {
	if len(rest) == 0 {
		return model.Op{}, fmt.Errorf("RENAME requires an argument")
	}
	switch {
	case rest[0].IsWord("COLUMN"):
		if len(rest) < 4 || !rest[2].IsWord("TO") {
			return model.Op{}, fmt.Errorf("RENAME COLUMN requires <old> TO <new>")
		}
		return model.Op{Kind: model.OpRenameColumn, Raw: raw, OldName: rest[1].Value, NewName: rest[3].Value}, nil
	case rest[0].IsWord("TO") || rest[0].IsWord("AS"):
		if len(rest) < 2 {
			return model.Op{}, fmt.Errorf("RENAME TO requires a name")
		}
		return model.Op{Kind: model.OpRenameTable, Raw: raw, NewName: rest[1].Value}, nil
	}
	return model.Op{Kind: model.OpUnknown, Raw: raw}, fmt.Errorf("unsupported RENAME form")
}

func parseAlterColumn(rest []Token, raw string) (model.Op, error) {
	if len(rest) > 0 && rest[0].IsWord("COLUMN") {
		rest = rest[1:]
	}
	if len(rest) < 2 || !rest[0].IsIdentifier() {
		return model.Op{}, fmt.Errorf("ALTER COLUMN requires a column name")
	}
	name := rest[0].Value
	rest = rest[1:]
	switch {
	case rest[0].IsWord("SET") && len(rest) > 1 && rest[1].IsWord("DEFAULT"):
		v := ""
		if len(rest) > 2 {
			v = rest[2].Value
			if rest[2].Kind == KindString {
				v = "'" + v + "'"
			}
		}
		return model.Op{Kind: model.OpSetDefault, Raw: raw, Column: &model.Column{Name: name}, DefaultValue: &v}, nil
	case rest[0].IsWord("DROP") && len(rest) > 1 && rest[1].IsWord("DEFAULT"):
		return model.Op{Kind: model.OpDropDefault, Raw: raw, Column: &model.Column{Name: name}}, nil
	}
	return model.Op{Kind: model.OpUnknown, Raw: raw}, fmt.Errorf("unsupported ALTER COLUMN form")
}

// columnModifierWords terminate the type portion of a column definition.
var columnModifierWords = map[string]bool{
	"NOT": true, "NULL": true, "DEFAULT": true, "AUTO_INCREMENT": true,
	"UNIQUE": true, "PRIMARY": true, "KEY": true, "COMMENT": true,
	"COLLATE": true, "CHARACTER": true, "GENERATED": true, "AS": true,
	"STORED": true, "VIRTUAL": true, "REFERENCES": true, "CHECK": true,
	"FIRST": true, "AFTER": true, "ON": true, "INVISIBLE": true,
	"VISIBLE": true, "SRID": true, "COLUMN_FORMAT": true, "STORAGE": true,
}

// parseColumnDef parses "<name> <type> [modifiers...] [FIRST|AFTER col]".
func parseColumnDef(tokens []Token) (model.Column, *model.ColumnPosition, error) {
	if len(tokens) == 0 {
		return model.Column{}, nil, fmt.Errorf("empty column definition")
	}
	if !tokens[0].IsIdentifier() {
		return model.Column{}, nil, fmt.Errorf("column name must be an identifier")
	}
	col := model.Column{Name: tokens[0].Value}
	i := 1

	if i >= len(tokens) || tokens[i].Kind != KindWord {
		return model.Column{}, nil, fmt.Errorf("column %q has no data type", col.Name)
	}

	base := strings.ToLower(tokens[i].Value)
	col.DataType = base
	var typ strings.Builder
	typ.WriteString(base)
	i++

	// Type parameters: (191), ('a','b'), etc. May be followed by UNSIGNED/ZEROFILL.
	if i < len(tokens) && tokens[i].IsPunct("(") {
		_, next, err := consumeParen(tokens, i)
		if err != nil {
			return model.Column{}, nil, err
		}
		var inner strings.Builder
		for _, t := range tokens[i:next] {
			inner.WriteString(t.Raw)
		}
		typ.WriteString(strings.TrimSpace(inner.String()))
		i = next
	}
	for i < len(tokens) && (tokens[i].IsWord("UNSIGNED") || tokens[i].IsWord("SIGNED") || tokens[i].IsWord("ZEROFILL")) {
		typ.WriteString(" " + strings.ToLower(tokens[i].Value))
		i++
	}
	col.Type = typ.String()

	var pos *model.ColumnPosition
	col.Nullable = true

	for i < len(tokens) {
		t := tokens[i]
		switch {
		case t.IsWord("NOT") && i+1 < len(tokens) && tokens[i+1].IsWord("NULL"):
			col.Nullable = false
			i += 2
		case t.IsWord("NULL"):
			col.Nullable = true
			i++
		case t.IsWord("DEFAULT"):
			i++
			if i < len(tokens) {
				switch {
				case tokens[i].IsPunct("("):
					_, next, err := consumeParen(tokens, i)
					if err != nil {
						return model.Column{}, nil, err
					}
					i = next
					s := "expr"
					col.Default = &s
				case tokens[i].IsPunct("-") && i+1 < len(tokens):
					v := "-" + tokens[i+1].Value
					col.Default = &v
					i += 2
				default:
					v := tokens[i].Value
					col.Default = &v
					i++
				}
			}
		case t.IsWord("CHARACTER") && i+1 < len(tokens) && tokens[i+1].IsWord("SET"):
			if i+2 < len(tokens) {
				col.Charset = strings.ToLower(tokens[i+2].Value)
			}
			i += 3
		case t.IsWord("COLLATE"):
			if i+1 < len(tokens) {
				col.Collation = strings.ToLower(tokens[i+1].Value)
			}
			i += 2
		case t.IsWord("AUTO_INCREMENT"):
			col.Extra = "auto_increment"
			i++
		case t.IsWord("GENERATED") || t.IsWord("AS"):
			col.Generated = true
			if col.Extra == "" {
				col.Extra = "GENERATED"
			}
			i++
			if i < len(tokens) && tokens[i].IsPunct("(") {
				_, next, err := consumeParen(tokens, i)
				if err != nil {
					return model.Column{}, nil, err
				}
				i = next
			}
		case t.IsWord("ON") && i+1 < len(tokens) && tokens[i+1].IsWord("UPDATE"):
			if i+2 < len(tokens) {
				col.OnUpdate = tokens[i+2].Value
			}
			i += 3
		case t.IsWord("FIRST"):
			pos = &model.ColumnPosition{First: true, Set: true}
			i++
		case t.IsWord("AFTER"):
			if i+1 < len(tokens) {
				pos = &model.ColumnPosition{After: tokens[i+1].Value, Set: true}
			}
			i += 2
		default:
			i++
		}
	}

	return col, pos, nil
}

// consumeParen returns the tokens inside the parenthesis group starting at index
// i (which must be an open paren), and the index just past the closing paren.
func consumeParen(tokens []Token, i int) (inner []Token, next int, err error) {
	if i >= len(tokens) || !tokens[i].IsPunct("(") {
		return nil, i, fmt.Errorf("expected '('")
	}
	depth := 0
	start := i + 1
	for j := i; j < len(tokens); j++ {
		switch {
		case tokens[j].IsPunct("("):
			depth++
		case tokens[j].IsPunct(")"):
			depth--
			if depth == 0 {
				return tokens[start:j], j + 1, nil
			}
		}
	}
	return nil, i, fmt.Errorf("unbalanced parentheses")
}
