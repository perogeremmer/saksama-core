package cred

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "my.cnf")
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	// os.WriteFile is subject to umask; force the mode we want to test.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveFromOptionFile(t *testing.T) {
	path := writeFile(t, `[client]
host = db.internal
port = 3307
user = app
password = hunter2
`, 0o600)

	c, err := Resolve(Options{DefaultsFile: path})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if c.Host != "db.internal" || c.Port != "3307" || c.User != "app" || c.Password != "hunter2" {
		t.Fatalf("bad connection: %+v", c)
	}
}

func TestOptionFilePasswordMustNotBeWorldReadable(t *testing.T) {
	path := writeFile(t, "[client]\nuser = app\npassword = hunter2\n", 0o644)

	_, err := Resolve(Options{DefaultsFile: path, AskPass: false})
	if err == nil {
		t.Fatal("expected a permission error for a group/world-readable credential file")
	}
	if !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("error should tell the user how to fix it, got: %v", err)
	}
}

func TestOptionFileWithoutPasswordMayBeReadable(t *testing.T) {
	path := writeFile(t, "[client]\nuser = app\nhost = db\n", 0o644)
	c, err := Resolve(Options{DefaultsFile: path})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if c.User != "app" {
		t.Fatalf("user = %q", c.User)
	}
}

func TestFlagsBeatEnvironmentBeatFile(t *testing.T) {
	path := writeFile(t, "[client]\nhost = from-file\nuser = file-user\npassword = file-pass\n", 0o600)

	t.Setenv("MYSQL_HOST", "from-env")
	t.Setenv("MYSQL_USER", "env-user")

	c, err := Resolve(Options{DefaultsFile: path, Host: "from-flag"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if c.Host != "from-flag" {
		t.Errorf("host = %q, want from-flag (flags win)", c.Host)
	}
	if c.User != "env-user" {
		t.Errorf("user = %q, want env-user (env beats file)", c.User)
	}
	if c.Password != "file-pass" {
		t.Errorf("password = %q, want file-pass (only the file has it)", c.Password)
	}
}

func TestPasswordFromEnvironment(t *testing.T) {
	t.Setenv("MYSQL_PWD", "env-secret")
	c, err := Resolve(Options{NoDefaults: true})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if c.Password != "env-secret" {
		t.Fatalf("password = %q", c.Password)
	}
}

func TestPromptOnlyWhenAskingAndMissing(t *testing.T) {
	var asked bool
	c, err := Resolve(Options{
		NoDefaults: true,
		AskPass:    true,
		PromptOverride: func(string) (string, error) {
			asked = true
			return "prompted", nil
		},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !asked || c.Password != "prompted" {
		t.Fatalf("prompt not used: asked=%v password=%q", asked, c.Password)
	}

	// Without --ask-pass there must be no prompt at all.
	asked = false
	if _, err := Resolve(Options{NoDefaults: true}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if asked {
		t.Fatal("prompted without --ask-pass")
	}
}

func TestPasswordReadFromPipedStdin(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		w.WriteString("piped-secret\n")
		w.Close()
	}()

	c, err := Resolve(Options{NoDefaults: true, AskPass: true, Stdin: r})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if c.Password != "piped-secret" {
		t.Fatalf("password = %q", c.Password)
	}
}

func TestDSNContainsPasswordButRedactedDoesNot(t *testing.T) {
	c := Conn{User: "app", Password: "hunter2", Host: "db", Port: "3306", Database: "shop"}

	dsn := c.DSN()
	if !strings.Contains(dsn, "hunter2") {
		t.Fatal("DSN must contain the password, otherwise the driver cannot authenticate")
	}
	if strings.Contains(c.RedactedDSN(), "hunter2") {
		t.Fatalf("RedactedDSN leaked the password: %s", c.RedactedDSN())
	}
	if !strings.Contains(c.RedactedDSN(), "app@tcp(db:3306)/shop") {
		t.Fatalf("unexpected redacted form: %s", c.RedactedDSN())
	}
}

func TestDSNSocketForm(t *testing.T) {
	c := Conn{User: "app", Password: "x", Socket: "/var/run/mysqld.sock", Database: "shop"}
	if !strings.Contains(c.DSN(), "unix(/var/run/mysqld.sock)") {
		t.Fatalf("socket DSN malformed: %s", c.DSN())
	}
}

func TestMaskSecret(t *testing.T) {
	cases := map[string]string{
		"":       "",
		"ab":     "**",
		"secret": "s****t",
	}
	for in, want := range cases {
		if got := MaskSecret(in); got != want {
			t.Errorf("MaskSecret(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDefaultPortAndUser(t *testing.T) {
	t.Setenv("MYSQL_HOST", "")
	t.Setenv("MYSQL_USER", "")
	t.Setenv("MYSQL_PWD", "")
	c, err := Resolve(Options{NoDefaults: true})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if c.Port != "3306" {
		t.Errorf("port = %q", c.Port)
	}
	if c.User != "root" {
		t.Errorf("user = %q", c.User)
	}
}

func TestOptionFileIgnoresOtherSections(t *testing.T) {
	path := writeFile(t, `[mysqld]
user = mysql-server-user
password = server-secret

[client]
user = client-user
`, 0o600)

	c, err := Resolve(Options{DefaultsFile: path})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if c.User != "client-user" {
		t.Fatalf("user = %q, want client-user (the [mysqld] section must be ignored)", c.User)
	}
	if c.Password != "" {
		t.Fatalf("password = %q, want empty", c.Password)
	}
}
