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

	// SHOW CREATE TABLE writes constraint enforcement inside a version comment:
	// `CONSTRAINT c CHECK ((x > 0)) /*!80016 NOT ENFORCED */`. Comments are dropped
	// by the tokenizer, so the marker is unfenced first - only this exact marker, so
	// no other comment in a dump changes meaning. It matters: NOT ENFORCED is the
	// difference between a copy that carries dirty rows and one that fails on them.
	sql = strings.ReplaceAll(sql, "/*!80016 NOT ENFORCED */", " NOT ENFORCED ")

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
	// An empty quoted identifier is a token, but not a table. The fuzzer found
	// `CREATE TABLE``(``A)0` parsing into a nameless table with a nameless column,
	// which MySQL rejects and which the analyzer would happily plan a change for.
	if strings.TrimSpace(tbl.Name) == "" {
		return tbl, nil, fmt.Errorf("empty table name")
	}
	if tbl.Schema != "" && strings.TrimSpace(tbl.Schema) == "" {
		return tbl, nil, fmt.Errorf("empty schema name")
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
			if len(def) < 2 {
				// `PRIMARY` on its own is a truncated definition, not a key. The
				// fuzzer reached def[2:] on a one-token slice and panicked.
				return tbl, nil, fmt.Errorf("truncated primary key definition: %q", JoinTokens(def))
			}
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
			switch {
			case len(tail) > 0 && tail[0].IsWord("FOREIGN"):
				fk, err := parseForeignKey(name, tail)
				if err != nil {
					return tbl, nil, fmt.Errorf("foreign key %q: %w", name, err)
				}
				tbl.ForeignKeys = append(tbl.ForeignKeys, *fk)
			case len(tail) > 0 && tail[0].IsWord("CHECK"):
				chk, err := parseCheck(name, tail)
				if err != nil {
					return tbl, nil, fmt.Errorf("check constraint %q: %w", name, err)
				}
				tbl.Checks = append(tbl.Checks, chk)
			default:
				ignored = append(ignored, "CONSTRAINT "+name)
			}

		case first.IsWord("FOREIGN"):
			fk, err := parseForeignKey("", def)
			if err != nil {
				return tbl, nil, fmt.Errorf("foreign key: %w", err)
			}
			tbl.ForeignKeys = append(tbl.ForeignKeys, *fk)

		case first.IsWord("CHECK"):
			chk, err := parseCheck("", def)
			if err != nil {
				return tbl, nil, fmt.Errorf("check constraint: %w", err)
			}
			tbl.Checks = append(tbl.Checks, chk)

		default:
			col, _, err := parseColumnDef(def)
			if err != nil {
				// A fragment the reader cannot see is not a finding. The tokenizer
				// treats some Unicode spaces as whitespace, and the fuzzer produced a
				// definition that rendered as "" and was reported as ignored.
				if text := strings.TrimSpace(JoinTokens(def)); text != "" {
					ignored = append(ignored, text)
				}
				continue
			}
			if strings.TrimSpace(col.Name) == "" {
				return tbl, nil, fmt.Errorf("column definition with no name: %q", JoinTokens(def))
			}
			tbl.Columns = append(tbl.Columns, col)
		}
	}

	// Two columns with one name is error 1060, so a definition that contains them
	// is not a table. Refusing here also keeps the analyzer honest: a model with a
	// duplicated name makes every column lookup ambiguous, and the reply would
	// depend on which entry the scan reached first.
	seenColumns := make(map[string]bool, len(tbl.Columns))
	for _, col := range tbl.Columns {
		key := strings.ToLower(col.Name)
		if seenColumns[key] {
			return tbl, nil, fmt.Errorf("duplicate column %q", col.Name)
		}
		seenColumns[key] = true
	}

	// An index with no parts is not an index. `FULLTEXT()` used to arrive here as a
	// real index over nothing, and the analyzer would then reason about a key that
	// does not exist. MySQL rejects the definition, so this does too.
	for _, idx := range tbl.Indexes {
		if len(idx.Parts) == 0 {
			return tbl, nil, fmt.Errorf("index %q has no columns", idx.Name)
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
		case tokens[i].IsWord("AUTO_INCREMENT"):
			// SHOW CREATE TABLE carries the next id. Reading it lets an offline plan
			// reason about the id space; the live path gets it from the catalogue.
			if v, ok := optionAfterEquals(tokens, i); ok {
				if n, err := strconv.ParseInt(strings.Trim(v, "`"), 10, 64); err == nil {
					tbl.AutoIncrement = n
				}
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

	// A table with no columns is not a table: MySQL rejects `CREATE TABLE t ()`,
	// so accepting it here would let the analyzer plan a change against nothing.
	// The fuzzer found this one too.
	if len(tbl.Columns) == 0 {
		return tbl, ignored, fmt.Errorf("table %q has no columns", tbl.Name)
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

// parseCheck reads a CHECK constraint from the tokens that start at CHECK.
//
// The expression is kept as the server wrote it, joined back into one string: it is
// what gets run as `SELECT 1 FROM t WHERE NOT (<expression>) LIMIT 1` to find rows
// that would break a copy, so it has to be valid SQL of its own.
func parseCheck(name string, tail []Token) (model.Check, error) {
	if len(tail) < 2 || !tail[0].IsWord("CHECK") || !tail[1].IsPunct("(") {
		return model.Check{}, fmt.Errorf("expected CHECK (expression)")
	}
	inner, next, err := consumeParen(tail, 1)
	if err != nil {
		return model.Check{}, err
	}
	if len(strings.TrimSpace(JoinTokens(inner))) == 0 {
		return model.Check{}, fmt.Errorf("CHECK has an empty expression")
	}
	// MySQL stores the expression with its own parentheses: CHECK ((`x` > 0)). One
	// layer is redundant for the probe query, which wraps the expression in NOT (...)
	// anyway, so unwrap while the whole expression is parenthesised.
	for {
		if len(inner) < 2 || !inner[0].IsPunct("(") || !inner[len(inner)-1].IsPunct(")") {
			break
		}
		sub, next, err := consumeParen(inner, 0)
		if err != nil || next != len(inner) {
			break
		}
		inner = sub
	}
	chk := model.Check{Name: name, Expression: JoinTokens(inner), Enforced: true}
	// 8.0.16 writes `NOT ENFORCED` after the expression; `ENFORCED` may appear too.
	rest := tail[next:]
	if len(rest) >= 2 && rest[0].IsWord("NOT") && rest[1].IsWord("ENFORCED") {
		chk.Enforced = false
	}
	return chk, nil
}
