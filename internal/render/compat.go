package render

import (
	"fmt"
	"io"
	"strings"

	"github.com/perogeremmer/saksama-core/internal/probe"
	"github.com/perogeremmer/saksama-core/internal/support"
)

// Compatibility renders the compatibility document.
//
// It is generated, not written by hand, so a row can never claim something the
// repository does not do.
func Compatibility(w io.Writer) error {
	var b strings.Builder

	b.WriteString("# Compatibility\n\n")
	b.WriteString("> Generated from `internal/support` by `make compat-doc`. Do not edit by hand;\n")
	b.WriteString("> edit the data and regenerate, or the test will fail.\n\n")

	b.WriteString("Every row states how much evidence backs it:\n\n")
	b.WriteString("- **verified**: a test in this repository ran against it\n")
	b.WriteString("- **claimed**: the version gates and capability rules encode it, but nothing here has run it end to end\n")
	b.WriteString("- **not yet**: the code path exists and is unit tested, but no real binary or server has touched it\n")
	b.WriteString("- **no**: refused on purpose\n\n")

	b.WriteString("## MySQL\n\n")
	b.WriteString("| Version | Verified | Native DDL that applies | Detail |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, row := range support.MySQL {
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n",
			row.Version, row.Status.Mark(), row.Native, row.Detail)
	}

	b.WriteString("\nNative DDL is the cheapest path and the one Saksama prefers. It needs neither a copy\n")
	b.WriteString("engine nor extra disk, but it still takes the metadata lock at the end, so a long\n")
	b.WriteString("statement can queue the whole server.\n")

	b.WriteString("\n## Engines\n\n")
	b.WriteString("| Engine | Version | Verified | Detail |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, row := range support.Engines {
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n",
			row.Engine, row.Version, row.Status.Mark(), row.Detail)
	}

	b.WriteString("\n## Hosts\n\n")
	b.WriteString("The binary is statically linked, so it runs on any Linux distribution. The *advice*\n")
	b.WriteString("is not portable: the Perl modules pt-osc needs are `libdbd-mysql-perl` on Debian and\n")
	b.WriteString("`perl-DBD-MySQL` on Fedora, and a wrong package name is a fix nobody can follow.\n\n")
	b.WriteString("| Host | Verified | Detail |\n")
	b.WriteString("|---|---|---|\n")
	for _, row := range support.Hosts {
		fmt.Fprintf(&b, "| %s | %s | %s |\n", row.OS, row.Status.Mark(), row.Detail)
	}

	b.WriteString("\n## Server settings `advise` reads\n\n")
	b.WriteString("A suitabile engine on a server that cannot start it is still a failed migration,\n")
	b.WriteString("so these are checked and shown with the value the server reports right now.\n\n")
	for _, setting := range support.ServerSettings {
		fmt.Fprintf(&b, "- %s\n", setting)
	}

	b.WriteString("\n## Findings\n\n")
	b.WriteString("Facts that cost someone an afternoon. They are here so the next person does not pay twice.\n\n")
	for _, f := range support.Findings {
		fmt.Fprintf(&b, "- %s\n", f)
	}

	b.WriteString("\n## What to do with an unverified combination\n\n")
	b.WriteString("Nothing here promises a combination that has not been tested. If you are on one:\n\n")
	b.WriteString("1. `saksama advise --live -d <db>` is safe: it only reads the schema.\n")
	b.WriteString("2. `saksama plan --live ...` is safe: it plans and refuses, and changes nothing.\n")
	b.WriteString("3. `saksama apply ... --dry-run` builds the exact argv or SQL and stops.\n")
	b.WriteString("4. Then read the plan, and only then run it without `--dry-run`.\n\n")
	b.WriteString("If it works, saying so is a genuine contribution: add the row to\n")
	b.WriteString("`internal/support/support.go` and run `make compat-doc`.\n")

	b.WriteString("\n## PostgreSQL\n\n")
	for _, line := range support.Postgres {
		fmt.Fprintf(&b, "- %s\n", line)
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// LocalCompatibility reports what is installed here and whether that combination
// is covered by the declared matrix.
func LocalCompatibility(w io.Writer, serverVersion string, binaries map[string]probe.Result) error {
	var b strings.Builder

	b.WriteString("LOCAL COMPATIBILITY\n")
	if serverVersion == "" {
		b.WriteString("  server   not inspected (use --live -h <host> -d <db>)\n")
	} else {
		mark := "not in the matrix"
		for _, row := range support.MySQL {
			if matchesVersion(row.Version, serverVersion) && row.Status == support.Verified {
				mark = "verified"
				break
			}
		}
		fmt.Fprintf(&b, "  %-8s MySQL %-12s %s\n", "server", serverVersion, mark)
	}

	if len(binaries) > 0 {
		names := []string{"gh-ost", "pt-online-schema-change"}
		for _, name := range names {
			r, ok := binaries[name]
			switch {
			case !ok || !r.Available:
				fmt.Fprintf(&b, "  %-8s %-24s not installed\n", name, "")
			default:
				mark := "not in the matrix"
				for _, row := range support.Engines {
					if row.Engine == engineKeyOf(name) && row.Status == support.Verified && strings.HasPrefix(r.Version, majorMinor(row.Version)) {
						mark = "verified"
						break
					}
				}
				fmt.Fprintf(&b, "  %-8s %-24s %s\n", name, r.Version, mark)
			}
		}
	}

	b.WriteString("\n  the full matrix: docs/COMPATIBILITY.md\n")
	b.WriteString("  safe first steps on an unverified combination:\n")
	b.WriteString("    saksama advise --live -d <db>        read only\n")
	b.WriteString("    saksama apply ... --dry-run          builds the command and stops\n")

	_, err := io.WriteString(w, b.String())
	return err
}

func engineKeyOf(binary string) string {
	if binary == "pt-online-schema-change" {
		return "pt-osc"
	}
	return binary
}

func majorMinor(v string) string {
	parts := strings.Split(v, ".")
	if len(parts) >= 2 {
		return parts[0] + "." + parts[1]
	}
	return v
}

func matchesVersion(marker, version string) bool {
	switch {
	case strings.HasSuffix(marker, ".x"):
		return strings.HasPrefix(version, strings.TrimSuffix(marker, ".x")+".")
	case strings.Contains(marker, " - "):
		bounds := strings.SplitN(marker, " - ", 2)
		return strings.HasPrefix(version, majorMinor(strings.TrimSpace(bounds[0])))
	case strings.HasSuffix(marker, "+"):
		base := strings.TrimSuffix(marker, "+")
		return strings.HasPrefix(version, base) || strings.Compare(version, base) >= 0
	}
	return strings.HasPrefix(version, marker)
}
