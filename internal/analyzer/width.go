package analyzer

import (
	"strconv"
	"strings"

	"github.com/perogeremmer/saksama-core/internal/model"
)

// charsetMaxLen is the maximum bytes per character for a MySQL character set.
// Unknown charsets return 4 (utf8mb4), which is the conservative upper bound
// for index-length estimation.
func charsetMaxLen(charset string) int {
	switch strings.ToLower(charset) {
	case "utf8mb4":
		return 4
	case "utf8", "utf8mb3":
		return 3
	case "ucs2", "utf16":
		return 4 // utf16 is 2 or 4; 4 is the safe bound
	case "utf32":
		return 4
	case "big5", "cp932", "euckr", "gb2312", "gbk", "sjis", "gb18030":
		return 2
	case "binary":
		return 1
	case "ascii", "latin1", "latin2", "latin5", "latin7", "greek", "hebrew",
		"cp1250", "cp1251", "cp1256", "cp1257", "koi8r", "koi8u",
		"armscii8", "dec8", "cp850", "cp852", "cp866", "geostd8",
		"keybcs2", "hp8", "roman", "swe7", "tis620":
		return 1
	}
	return 4
}

// columnMaxBytes estimates the maximum on-disk width of a column in an index.
func columnMaxBytes(c model.Column) int {
	t := strings.ToLower(c.DataType)
	arg := typeArg(c.Type)

	mb := charsetMaxLen(c.Charset)
	if c.Charset == "" {
		mb = 4
	}

	switch t {
	case "char", "varchar":
		if arg > 0 {
			return arg * mb
		}
		return 255 * mb
	case "tinytext":
		return 255
	case "text":
		return 65535
	case "mediumtext":
		return 16777215
	case "longtext":
		return 4294967295
	case "tinyblob":
		return 255
	case "blob":
		return 65535
	case "mediumblob":
		return 16777215
	case "longblob":
		return 4294967295
	case "enum", "set":
		return 2
	case "tinyint":
		return 1
	case "smallint":
		return 2
	case "mediumint":
		return 3
	case "int", "integer", "float":
		return 4
	case "bigint", "double", "real":
		return 8
	case "decimal", "numeric":
		if arg > 0 {
			return arg/2 + 2
		}
		return 16
	case "bit":
		if arg > 0 {
			return (arg + 7) / 8
		}
		return 8
	case "date", "time":
		return 3
	case "datetime", "timestamp":
		return 8
	case "year":
		return 1
	case "json":
		return 4294967295
	case "binary", "varbinary":
		if arg > 0 {
			return arg
		}
		return 255
	}
	return 8
}

// typeArg extracts the numeric argument from a normalized type such as
// "varchar(191)" or "decimal(18,2)". Returns 0 when absent or non-numeric.
func typeArg(typ string) int {
	open := strings.IndexByte(typ, '(')
	closeIdx := strings.IndexByte(typ, ')')
	if open < 0 || closeIdx <= open {
		return 0
	}
	inner := typ[open+1 : closeIdx]
	if i := strings.IndexByte(inner, ','); i >= 0 {
		inner = inner[:i]
	}
	n, err := strconv.Atoi(strings.TrimSpace(inner))
	if err != nil {
		return 0
	}
	return n
}

// indexKeyBytes computes the total key length, in bytes, of an index.
func indexKeyBytes(ix model.Index, tbl model.Table) int {
	total := 0
	for _, p := range ix.Parts {
		col, ok := tbl.Column(p.Column)
		if !ok {
			// Unknown column: assume a conservative 255 utf8mb4 bytes.
			total += 255 * 4
			continue
		}
		if p.PrefixLen > 0 {
			mb := charsetMaxLen(col.Charset)
			if col.Charset == "" {
				mb = 4
			}
			if col.IsTextual() {
				total += p.PrefixLen * mb
			} else {
				total += p.PrefixLen
			}
			continue
		}
		total += columnMaxBytes(col)
	}
	return total
}

// keyLimitBytes is the InnoDB maximum index key length for the row format.
func keyLimitBytes(rowFormat string) int {
	switch strings.ToUpper(rowFormat) {
	case "COMPACT", "REDUNDANT":
		return 767
	}
	return 3072
}
