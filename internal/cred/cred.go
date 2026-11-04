// Package cred resolves MySQL credentials without ever putting a password in
// argv, in an event, or in an audit export.
//
// Precedence: explicit flags > environment > option files. Option files are
// honoured because that is what every operator already uses, and because a
// password that lives in a 0600 file is a password that never appears in `ps`.
package cred

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/term"
)

// Conn is a fully resolved connection.
type Conn struct {
	Host     string
	Port     string
	User     string
	Password string
	Socket   string
	TLS      string
	Database string
}

// Options are the raw inputs from the command line and environment.
type Options struct {
	Host     string
	Port     string
	User     string
	TLS      string
	Database string

	// DefaultsFile is an explicit --defaults-extra-file.
	DefaultsFile string
	// NoDefaults disables reading ~/.my.cnf, for hermetic CI runs.
	NoDefaults bool
	// AskPass prompts on a TTY when no password was found anywhere.
	AskPass bool
	// Stdin is used for the prompt.
	Stdin io.Reader
	// PromptOverride replaces the terminal prompt, for tests.
	PromptOverride func(prompt string) (string, error)
}

// Resolve produces a connection.
func Resolve(o Options) (Conn, error) {
	c := Conn{Host: "127.0.0.1", Port: "3306"}

	// 1. Option files (lowest precedence).
	fileCreds, err := fromOptionFiles(o)
	if err != nil {
		return c, err
	}

	// 2. Environment.
	env := Conn{
		Host:     os.Getenv("MYSQL_HOST"),
		Port:     os.Getenv("MYSQL_TCP_PORT"),
		User:     os.Getenv("MYSQL_USER"),
		Password: os.Getenv("MYSQL_PWD"),
		Socket:   os.Getenv("MYSQL_UNIX_PORT"),
		Database: os.Getenv("MYSQL_DATABASE"),
	}

	// 3. Flags (highest precedence).
	c = merge(c, fileCreds)
	c = merge(c, env)
	c.Port = firstNonEmpty(o.Port, c.Port)
	c.Host = firstNonEmpty(o.Host, c.Host)
	c.User = firstNonEmpty(o.User, c.User)
	c.Database = firstNonEmpty(o.Database, c.Database)
	c.TLS = firstNonEmpty(o.TLS, "preferred")

	if c.User == "" {
		c.User = "root"
	}

	// 4. Prompt only if nothing was found.
	if c.Password == "" && o.AskPass {
		pw, err := promptPassword(o)
		if err != nil {
			return c, err
		}
		c.Password = pw
	}

	return c, nil
}

func merge(base, other Conn) Conn {
	return Conn{
		Host:     firstNonEmpty(other.Host, base.Host),
		Port:     firstNonEmpty(other.Port, base.Port),
		User:     firstNonEmpty(other.User, base.User),
		Password: firstNonEmpty(other.Password, base.Password),
		Socket:   firstNonEmpty(other.Socket, base.Socket),
		TLS:      firstNonEmpty(other.TLS, base.TLS),
		Database: firstNonEmpty(other.Database, base.Database),
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func promptPassword(o Options) (string, error) {
	if o.PromptOverride != nil {
		return o.PromptOverride("MySQL password: ")
	}
	if o.Stdin != nil {
		if f, ok := o.Stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
			fmt.Fprint(os.Stderr, "MySQL password: ")
			b, err := term.ReadPassword(int(f.Fd()))
			fmt.Fprintln(os.Stderr)
			if err != nil {
				return "", fmt.Errorf("reading password: %w", err)
			}
			return string(b), nil
		}
		// Non-terminal stdin: read one line. That is what `echo pw | saksama` does.
		br := bufio.NewReader(o.Stdin)
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return "", err
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	return "", errors.New("no password found and --ask-pass was given without a terminal")
}

func fromOptionFiles(o Options) (Conn, error) {
	var paths []string
	if o.DefaultsFile != "" {
		paths = []string{o.DefaultsFile}
	} else if !o.NoDefaults {
		if h, err := os.UserHomeDir(); err == nil {
			paths = append(paths, filepath.Join(h, ".my.cnf"))
		}
		if v := os.Getenv("MYSQL_HOME"); v != "" {
			paths = append(paths, filepath.Join(v, "my.cnf"))
		}
		paths = append(paths, "/etc/my.cnf")
	}

	c := Conn{}
	for _, p := range paths {
		creds, found, err := readOptionFile(p)
		if err != nil {
			return c, err
		}
		if found {
			c = merge(creds, c)
		}
	}
	return c, nil
}

// readOptionFile parses the [client] and [mysql] groups.
func readOptionFile(path string) (Conn, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Conn{}, false, nil
		}
		// Unreadable /etc/my.cnf is normal for non-root users.
		return Conn{}, false, nil
	}
	defer f.Close()

	c := Conn{}
	found := false
	section := ""
	hasPassword := false

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.Trim(line, "[]"))
			continue
		}
		if section != "client" && section != "mysql" {
			continue
		}
		key, value, ok := splitOption(line)
		if !ok {
			continue
		}
		found = true
		switch key {
		case "host":
			c.Host = value
		case "port":
			c.Port = value
		case "user":
			c.User = value
		case "password", "passwd":
			c.Password = value
			hasPassword = true
		case "socket":
			c.Socket = value
		case "database":
			c.Database = value
		}
	}
	if err := scanner.Err(); err != nil {
		return Conn{}, false, err
	}

	if hasPassword {
		if err := checkPermissions(path); err != nil {
			return Conn{}, false, err
		}
	}

	return c, found, nil
}

func splitOption(line string) (key, value string, ok bool) {
	if i := strings.IndexByte(line, '='); i > 0 {
		return strings.ToLower(strings.TrimSpace(line[:i])), unquote(strings.TrimSpace(line[i+1:])), true
	}
	fields := strings.Fields(line)
	if len(fields) >= 2 {
		return strings.ToLower(fields[0]), unquote(strings.Join(fields[1:], " ")), true
	}
	return "", "", false
}

func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// checkPermissions refuses a credential file that other users can read. A
// password in a world-readable file is a security incident waiting to happen, so
// this is an error rather than a warning.
func checkPermissions(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	mode := info.Mode().Perm()
	if mode&0o077 != 0 {
		return fmt.Errorf(
			"%s contains a password but is readable by others (mode %#o). Fix it first: chmod 600 %s",
			path, mode, path)
	}
	return nil
}

// DSN builds a go-sql-driver/mysql DSN. Never log the result: use RedactedDSN.
func (c Conn) DSN() string {
	if c.Socket != "" {
		return fmt.Sprintf("%s:%s@unix(%s)/%s?parseTime=true&loc=UTC&timeout=10s&readTimeout=0&writeTimeout=0&tls=%s",
			c.User, c.Password, c.Socket, c.Database, c.tls())
	}
	return fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&loc=UTC&timeout=10s&readTimeout=0&writeTimeout=0&tls=%s&interpolateParams=true",
		c.User, c.Password, c.Host, c.Port, c.Database, c.tls())
}

func (c Conn) tls() string {
	if c.TLS == "" {
		return "preferred"
	}
	return c.TLS
}

// RedactedDSN is safe to print and to store in an audit log.
func (c Conn) RedactedDSN() string {
	if c.Socket != "" {
		return fmt.Sprintf("%s@unix(%s)/%s", c.User, c.Socket, c.Database)
	}
	return fmt.Sprintf("%s@tcp(%s:%s)/%s", c.User, c.Host, c.Port, c.Database)
}

// Describe returns a stable label for reports.
func (c Conn) Describe() string {
	if c.Socket != "" {
		return c.Socket
	}
	return c.Host
}

// MaskSecret is used by tests and renderers that must show a length hint but not
// the secret itself.
func MaskSecret(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 2 {
		return strings.Repeat("*", len(s))
	}
	return s[:1] + strings.Repeat("*", len(s)-2) + s[len(s)-1:]
}

// OptionFileKeys is used by the documentation and by tests to keep the supported
// key list honest.
func OptionFileKeys() []string {
	keys := []string{"host", "port", "user", "password", "passwd", "socket", "database"}
	sort.Strings(keys)
	return keys
}
