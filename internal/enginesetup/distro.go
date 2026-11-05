package enginesetup

import (
	"os"
	"runtime"
	"strings"
)

// Packages is how this host installs the Perl modules pt-online-schema-change
// loads at startup.
//
// The names are not portable and cannot be guessed from the Go runtime: the same
// two modules are `libdbd-mysql-perl` on Debian, `perl-DBD-MySQL` on Fedora,
// `perl-dbd-mysql` on Arch, and `perl-dbd-mysql` alongside `perl-dbi` on Alpine.
// Printing an apt-get line on a Fedora box is advice that cannot be followed, which
// is worse than saying nothing.
type Packages struct {
	// ID is the distribution family: debian, fedora, arch, alpine, suse, darwin,
	// or unknown.
	ID string
	// Install is the command prefix, e.g. "apt-get install". Empty when the family
	// is unknown, so no invented command is printed.
	Install string
	// Perl is the package that provides the interpreter.
	Perl string
	// DBD is the package that provides DBD::mysql (and, on most families, DBI with it).
	DBD string
	// Note is the honest fallback when the family is unknown.
	Note string
}

// Host is the package information for the machine running this build.
func Host() Packages {
	raw := ""
	if runtime.GOOS == "linux" {
		// Best effort: a container or a stripped image may have no os-release, and
		// that is exactly the case the fallback text exists for.
		if b, err := os.ReadFile("/etc/os-release"); err == nil {
			raw = string(b)
		}
	}
	return HostFrom(runtime.GOOS, raw)
}

// HostFrom is Host with the inputs supplied, so the mapping is testable without a
// container per distribution.
func HostFrom(goos, osRelease string) Packages {
	if goos == "darwin" {
		return Packages{
			ID:   "darwin",
			Perl: "perl",
			DBD:  "DBD::mysql",
			Note: "install DBD::mysql for the system perl: `cpan DBD::mysql`, or your package manager if it packages it",
		}
	}
	if goos != "linux" {
		return Packages{ID: "unknown", Note: unknownNote}
	}

	id, like := parseOSRelease(osRelease)
	family := classify(id, like)

	switch family {
	case "debian":
		return Packages{ID: "debian", Install: "apt-get install",
			Perl: "perl", DBD: "libdbi-perl libdbd-mysql-perl"}
	case "fedora":
		return Packages{ID: "fedora", Install: "dnf install",
			Perl: "perl", DBD: "perl-DBD-MySQL"}
	case "arch":
		return Packages{ID: "arch", Install: "pacman -S --needed",
			Perl: "perl", DBD: "perl-dbi perl-dbd-mysql"}
	case "alpine":
		return Packages{ID: "alpine", Install: "apk add",
			Perl: "perl", DBD: "perl-dbi perl-dbd-mysql"}
	case "suse":
		return Packages{ID: "suse", Install: "zypper install",
			Perl: "perl", DBD: "perl-DBD-mysql"}
	}
	return Packages{ID: "linux", Note: unknownNote}
}

const unknownNote = "install perl with the DBI and DBD::mysql modules for this distribution; no package name is guessed here"

// FixPerl is the command that installs the interpreter, or the honest note when the
// distribution is not recognised.
func (p Packages) FixPerl() string {
	if p.Install == "" || p.Perl == "" {
		return p.Note
	}
	return p.Install + " " + p.Perl
}

// FixDBD is the command that installs DBD::mysql (and DBI), or the honest note.
func (p Packages) FixDBD() string {
	if p.Install == "" || p.DBD == "" {
		return p.Note
	}
	return p.Install + " " + p.DBD
}

// Label is the one-phrase description shown beside the platform check.
func (p Packages) Label() string {
	if p.Install == "" {
		return p.ID
	}
	return p.ID + ", " + strings.Fields(p.Install)[0]
}

// parseOSRelease pulls ID and ID_LIKE out of /etc/os-release.
func parseOSRelease(raw string) (id, like string) {
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch key {
		case "ID":
			id = strings.ToLower(value)
		case "ID_LIKE":
			like = strings.ToLower(value)
		}
	}
	return id, like
}

// classify maps the identifiers to a package-manager family. ID wins; ID_LIKE is
// the fallback, because derivatives (Linux Mint, Rocky, Manjaro, EndeavourOS)
// inherit their parent's packaging.
func classify(id, like string) string {
	exact := map[string]string{
		"debian": "debian", "ubuntu": "debian",
		"fedora": "fedora", "rhel": "fedora", "centos": "fedora",
		"rocky": "fedora", "almalinux": "fedora", "amzn": "fedora",
		"arch": "arch", "manjaro": "arch", "endeavouros": "arch",
		"alpine": "alpine",
		"suse":   "suse", "opensuse": "suse", "opensuse-leap": "suse",
		"opensuse-tumbleweed": "suse", "sles": "suse", "sled": "suse",
	}
	if family, ok := exact[id]; ok {
		return family
	}
	for _, token := range strings.Fields(like) {
		if family, ok := exact[token]; ok {
			return family
		}
		if token == "opensuse" || token == "suse" {
			return "suse"
		}
	}
	return ""
}
