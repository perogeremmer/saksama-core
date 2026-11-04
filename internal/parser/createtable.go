package parser

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/perogeremmer/saksama-core/internal/model"
)

// ParseCreateTable parses the output of SHOW CREATE TABLE (or an equivalent
// CREATE TABLE statement) into a model.Table.
//
// It is used for offline planning, so it must not depend on a live connection.
// Unsupported fragments are ignored rather than guessed, and the caller is told
// what was ignored so the analyzer can warn about incomplete schema knowledge.
func ParseCreateTable(sql string) (model.Table, []string, error) {
	var tbl model.Table
	var ignored []string

	tokens, err := Tokenize(sql)
	if err != nil {
		return tbl, nil, err
	}
	tokens = TrimSemicolons(tokens)
	if err := RejectMultiStatement(tokens); err != nil {
		return tbl, nil, err
	}

	i := 0
	if i >= len(tokens) || !tokens[i].IsWord("CREATE") {
		return tbl, nil, fmt.Errorf("statement must start with CREATE")
	}
	i++
	if i < len(tokens) && tokens[i].IsWord("TEMPORARY") {
		i++
	}
	if i >= len(tokens) || !tokens[i].IsWord("TABLE") {
		return tbl, nil, fmt.Errorf("expected CREATE TABLE")
	}
	i++
	if i+2 < len(tokens) && tokens[i].IsWord("IF") && tokens[i+1].IsWord("NOT") && tokens[i+2].IsWord("EXISTS") {
		i += 3
	}

	if i >= len(tokens) || !tokens[i].IsIdentifier() {
		return tbl, nil, fmt.Errorf("expected table name")
	}
	tbl.Name = tokens[i].Value
	i++
	if i < len(tokens) && tokens[i].IsPunct(".") {
		if i+1 >= len(tokens) || !tokens[i+1].IsIdentifier() {
			return tbl, nil, fmt.Errorf("expected table name after qualifier")
		}
		tbl.Schema = tbl.Name
		tbl.Name = tokens[i+1].Value
		i += 2
	}

	if i >= len(tokens) || !tokens[i].IsPunct("(") {
		return tbl, nil, fmt.Errorf("expected column definitions")
	}
	inner, next, err := consumeParen(tokens, i)
	if err != nil {
		return tbl, nil, err
	}
	i = next

	for _, def := range SplitTopLevel(inner) {
		if len(def) == 0 {
			continue
		}
		first := def[0]

		switch {
		case first.IsWord("PRIMARY"):
			parts, _, err := parseIndexParts(def[2:]) // PRIMARY KEY (...)
			if err != nil {
				return tbl, nil, fmt.Errorf("primary key: %w", err)
			}
			tbl.Indexes = append(tbl.Indexes, model.Index{Name: "PRIMARY", Unique: true, Primary: true, Parts: parts})

		case first.IsWord("UNIQUE"):
			tail := def[1:]
			if len(tail) > 0 && (tail[0].IsWord("KEY") || tail[0].IsWord("INDEX")) {
				tail = tail[1:]
			}
			name, parts, err := parseIndexLine(tail)
			if err != nil {
				return tbl, nil, fmt.Errorf("unique key: %w", err)
			}
			tbl.Indexes = append(tbl.Indexes, model.Index{Name: name, Unique: true, Parts: parts})

		case first.IsWord("KEY") || first.IsWord("INDEX"):
			name, parts, err := parseIndexLine(def[1:])
			if err != nil {
				return tbl, nil, fmt.Errorf("key: %w", err)
			}
			tbl.Indexes = append(tbl.Indexes, model.Index{Name: name, Parts: parts})

		case first.IsWord("FULLTEXT"):
			tail := def[1:]
			if len(tail) > 0 && (tail[0].IsWord("KEY") || tail[0].IsWord("INDEX")) {
				tail = tail[1:]
			}
			name, parts, err := parseIndexLine(tail)
			if err != nil {
				return tbl, nil, fmt.Errorf("fulltext key: %w", err)
			}
			tbl.Indexes = append(tbl.Indexes, model.Index{Name: name, Fulltext: true, Parts: parts})

		case first.IsWord("SPATIAL"):
			tail := def[1:]
			if len(tail) > 0 && (tail[0].IsWord("KEY") || tail[0].IsWord("INDEX")) {
				tail = tail[1:]
			}
			name, parts, err := parseIndexLine(tail)
			if err != nil {
				return tbl, nil, fmt.Errorf("spatial key: %w", err)
			}
			tbl.Indexes = append(tbl.Indexes, model.Index{Name: name, Spatial: true, Parts: parts})

		case first.IsWord("CONSTRAINT"):
			name := ""
			tail := def[1:]
			if len(tail) > 0 && tail[0].IsIdentifier() && !tail[0].IsWord("FOREIGN") && !tail[0].IsWord("CHECK") && !tail[0].IsWord("PRIMARY") && !tail[0].IsWord("UNIQUE") {
				name = tail[0].Value
				tail = tail[1:]
			}
			if len(tail) > 0 && tail[0].IsWord("FOREIGN") {
				fk, err := parseForeignKey(name, tail)
				if err != nil {
					return tbl, nil, fmt.Errorf("foreign key %q: %w", name, err)
				}
				tbl.ForeignKeys = append(tbl.ForeignKeys, *fk)
			} else {
				ignored = append(ignored, "CONSTRAINT "+name)
			}

		case first.IsWord("FOREIGN"):
			fk, err := parseForeignKey("", def)
			if err != nil {
				return tbl, nil, fmt.Errorf("foreign key: %w", err)
			}
			tbl.ForeignKeys = append(tbl.ForeignKeys, *fk)

		case first.IsWord("CHECK"):
			ignored = append(ignored, "CHECK constraint")

		default:
			col, _, err := parseColumnDef(def)
			if err != nil {
				ignored = append(ignored, JoinTokens(def))
				continue
			}
			tbl.Columns = append(tbl.Columns, col)
		}
	}

	// Table options after the closing paren.
	for ; i < len(tokens); i++ {
		switch {
		case tokens[i].IsWord("ENGINE"):
			if v, ok := optionAfterEquals(tokens, i); ok {
				tbl.Engine = v
			}
		case tokens[i].IsWord("ROW_FORMAT"):
			if v, ok := optionAfterEquals(tokens, i); ok {
				tbl.RowFormat = strings.ToUpper(v)
			}
		case tokens[i].IsWord("CHARSET"):
			if v, ok := optionAfterEquals(tokens, i); ok {
				tbl.Charset = strings.ToLower(v)
			}
		case tokens[i].IsWord("CHARACTER") && i+1 < len(tokens) && tokens[i+1].IsWord("SET"):
			if v, ok := optionAfterEquals(tokens, i+1); ok {
				tbl.Charset = strings.ToLower(v)
			}
		case tokens[i].IsWord("COLLATE"):
			if v, ok := optionAfterEquals(tokens, i); ok {
				tbl.Collation = strings.ToLower(v)
			}
		}
	}

	if tbl.Engine == "" {
		tbl.Engine = "InnoDB"
	}

	// Resolve the inherited charset/collation for every column so checks can
	// reason about byte widths without a live server.
	for idx := range tbl.Columns {
		c := &tbl.Columns[idx]
		if c.IsTextual() || c.DataType == "enum" || c.DataType == "set" {
			if c.Charset == "" {
				c.Charset = tbl.Charset
			}
			if c.Collation == "" {
				c.Collation = tbl.Collation
			}
		}
	}

	return tbl, ignored, nil
}

func optionAfterEquals(tokens []Token, i int) (string, bool) {
	j := i + 1
	if j < len(tokens) && tokens[j].IsPunct("=") {
		j++
	}
	if j < len(tokens) && (tokens[j].Kind == KindWord || tokens[j].Kind == KindIdent || tokens[j].Kind == KindNumber) {
		return tokens[j].Value, true
	}
	return "", false
}

func parseIndexLine(tokens []Token) (string, []model.IndexPart, error) {
	name := ""
	tokens = trimLeadingEquals(tokens)
	if len(tokens) > 0 && tokens[0].IsIdentifier() && !tokens[0].IsWord("USING") && !tokens[0].IsPunct("(") {
		name = tokens[0].Value
		tokens = tokens[1:]
	}
	parts, _, err := parseIndexParts(tokens)
	if err != nil {
		return name, nil, err
	}
	if name == "" && len(parts) > 0 {
		name = parts[0].Column // MySQL names the index after the first column
	}
	return name, parts, nil
}

// ParseInt64 parses a decimal integer with validation.
func ParseInt64(s string) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(s), 10, 64)
}
