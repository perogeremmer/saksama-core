package analyzer

import (
	"fmt"
)

// DiskFacts is what is known about the volume holding the data directory.
//
// MySQL cannot report free filesystem space over SQL, so this is either measured
// on the database host or supplied by the operator. Anything else is unknown, and
// unknown is reported as unknown rather than assumed safe.
type DiskFacts struct {
	Free     int64 `json:"free_bytes"`
	Total    int64 `json:"total_bytes"`
	Measured bool  `json:"measured"`
	// Source says where the number came from: "filesystem" or "operator".
	Source string `json:"source,omitempty"`
	// Reason explains an unmeasured value.
	Reason string `json:"reason,omitempty"`
	// Path and ServerHost answer "where would I look": the data directory, and what
	// the server calls itself. A container reports its id or service name there,
	// which is what a docker command needs.
	Path       string `json:"path,omitempty"`
	ServerHost string `json:"server_host,omitempty"`
}

// FreePercent is the free share of the volume, or 0 when unmeasured.
func (d DiskFacts) FreePercent() float64 {
	if !d.Measured || d.Total <= 0 {
		return 0
	}
	return float64(d.Free) / float64(d.Total) * 100
}

// Disk thresholds. The low water mark exists because a migration that fits today
// can still starve the server it is running on: binlogs, undo, and the rest of the
// application keep writing while the copy runs.
const (
	// DiskLowWaterPercent is the minimum free share of the volume.
	DiskLowWaterPercent = 10.0
	// DiskWarnFreePercent is where the plan starts warning.
	DiskWarnFreePercent = 25.0
	// DiskCopyFactor is how much extra space a copy engine needs, as a multiple of
	// the table size: the copy table plus the retained original.
	DiskCopyFactor = 2
)

// containerDiskHint names the command that answers the disk question when the
// server is a container. This is a common setup - a MySQL container on the same
// machine - and the honest options there are to supply the numbers or to read them
// from the container. The hint is printed, never run: a migration tool has no
// business starting processes on the operator's behalf.
func containerDiskHint(d DiskFacts) string {
	if d.ServerHost == "" {
		return ""
	}
	path := d.Path
	if path == "" {
		path = "/var/lib/mysql"
	}
	if isContainerID(d.ServerHost) {
		return fmt.Sprintf("this looks like a container (the server calls itself %q): docker exec %s df -B1 %s",
			d.ServerHost, d.ServerHost, path)
	}
	return fmt.Sprintf("if the server is a container (it calls itself %q): docker exec %s df -B1 %s",
		d.ServerHost, d.ServerHost, path)
}

// isContainerID reports the 12- or 64-hex shape Docker uses for container ids.
func isContainerID(s string) bool {
	if len(s) != 12 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// diskChecks evaluates D6: is there room to run this, and is the volume healthy.
//
// It runs for every plan, not only for rebuilds: an INSTANT change writes almost
// nothing, but a volume with 2 percent free is a problem regardless, and a
// migration is exactly the wrong time to discover it.
func diskChecks(in Input, copiesByOp bool, engine Engine) []Check {
	// A copy engine copies the table whatever the operations need, so the space
	// question depends on the engine too - and the reason matters to the reader: a
	// change that needs a copy is a different situation from a change that only
	// copies because an engine was chosen for it.
	copiesByEngine := engine == EngineGhOst || engine == EnginePtOSC
	needsCopy := copiesByOp || copiesByEngine
	d := in.Disk
	if !d.Measured {
		if in.FreeDisk > 0 {
			// The operator supplied a free figure without a volume size. We can
			// still judge whether it is enough.
			d = DiskFacts{Free: in.FreeDisk, Measured: true, Source: "operator"}
		} else if in.Disk.Free > 0 {
			d = DiskFacts{Free: in.Disk.Free, Measured: true, Source: "operator"}
		}
	}

	size := in.Table.SizeBytes()
	need := size * DiskCopyFactor

	if !d.Measured {
		detail := "free disk space could not be measured: " + fallbackReason(d.Reason)
		if needsCopy && size > 0 {
			detail += fmt.Sprintf(". A copy needs roughly %s extra (the copy table plus the retained original)",
				humanBytes(need))
		}
		// The two ways to answer the question honestly: give the numbers, or run
		// where the numbers live. When the server is a container on this machine,
		// "run it on the host" is not the only option - one command reads the
		// container's own filesystem, and that is the situation people hit.
		remediation := "pass --free-disk and --disk-total, or run Saksama on the database host so the filesystem can be read"
		if hint := containerDiskHint(d); hint != "" {
			remediation += ". " + hint
		}
		if !copiesByOp && copiesByEngine {
			// The change itself copies nothing; the chosen engine does. That is
			// avoidable, and saying so is the difference between "buy more disk"
			// and "you do not need this engine here".
			detail += fmt.Sprintf(". This change does not copy the table, but %s does, because it was chosen for this target", engine)
			remediation = "clear the recorded engine choice with `saksama engine clear` (or pass --engine native) to do this without a copy, or " + remediation
		}
		if !needsCopy {
			// This change does not copy the table, so the space a copy needs is not
			// required for it. Blocking an ADD INDEX because a figure we never
			// needed is missing is the same mistake as demanding a package install
			// for something MySQL does itself: the fact does not apply, so its
			// absence must not stop the run.
			return []Check{{
				ID: chkDiskHeadroom, Severity: SeverityWarn,
				Title:       "free disk space could not be measured",
				Detail:      detail + ". This change does not copy the table, so the space a copy needs is not required for it",
				Remediation: "none needed for this change; pass --free-disk and --disk-total before a change that rebuilds or copies the table",
			}}
		}
		if !in.ForExecution {
			// Planning is useful without a measured disk; executing is not.
			return []Check{{
				ID: chkDiskHeadroom, Severity: SeverityWarn,
				Title:       "free disk space could not be measured",
				Detail:      detail,
				Remediation: remediation,
			}}
		}
		remediation += ", or --force to run with an unmeasured volume (recorded in the audit log)"
		if in.Force {
			return []Check{{
				ID: chkDiskHeadroom, Severity: SeverityWarn,
				Title:       "free disk space could not be measured (allowed by --force)",
				Detail:      detail,
				Remediation: remediation,
			}}
		}
		return []Check{{
			ID: chkDiskHeadroom, Severity: SeverityBlocker,
			Title:       "free disk space could not be measured",
			Detail:      detail,
			Remediation: remediation,
		}}
	}

	var out []Check

	// 1. Is there room for this specific change.
	if needsCopy && size > 0 && d.Free < need {
		out = append(out, Check{
			ID: chkDiskHeadroom, Severity: SeverityBlocker,
			Title:       "not enough free space for the copy",
			Detail:      fmt.Sprintf("%s free but %s required", humanBytes(d.Free), humanBytes(need)),
			Remediation: "free space before planning again: a copy engine builds a full second table before it swaps, so it needs the table's data length free, not a fraction of it. Where the space went: du -xh --max-depth=1 <datadir>, and PURGE BINARY LOGS TO '<file>' reclaims binlogs you no longer need on a replica",
		})
	}

	// 2. Is the volume itself healthy. This applies to every migration, including
	//    ones that write almost nothing, because the copy is not the only writer.
	if d.Total > 0 {
		pct := d.FreePercent()
		switch {
		case pct < DiskLowWaterPercent:
			out = append(out, Check{
				ID: chkDiskHeadroom, Severity: SeverityBlocker,
				Title: fmt.Sprintf("volume is below %.0f%% free", DiskLowWaterPercent),
				Detail: fmt.Sprintf("%s of %s free (%.1f%%), below the %.0f%% floor",
					humanBytes(d.Free), humanBytes(d.Total), pct, DiskLowWaterPercent),
				Remediation: "get back above the floor before starting: binlogs, undo, and ordinary writes keep growing during a migration, so the free space you have now is not the free space you will have at cut-over. Check what is growing: du -xh --max-depth=1 <datadir>",
			})
		case pct < DiskWarnFreePercent:
			out = append(out, Check{
				ID: chkDiskHeadroom, Severity: SeverityWarn,
				Title: fmt.Sprintf("volume is at %.1f%% free", pct),
				Detail: fmt.Sprintf("%s of %s free",
					humanBytes(d.Free), humanBytes(d.Total)),
				Remediation: fmt.Sprintf("below %.0f%% free is refused; free space before a long copy", DiskLowWaterPercent),
			})
		}
	}

	if len(out) == 0 {
		detail := fmt.Sprintf("%s free", humanBytes(d.Free))
		if d.Total > 0 {
			detail = fmt.Sprintf("%s of %s free (%.1f%%)", humanBytes(d.Free), humanBytes(d.Total), d.FreePercent())
		}
		if needsCopy && size > 0 {
			detail += fmt.Sprintf(", %s required for the copy", humanBytes(need))
		}
		if d.Source == "operator" {
			detail += " (supplied with --free-disk)"
		}
		out = append(out, Check{
			ID: chkDiskHeadroom, Severity: SeverityInfo,
			Title: "disk headroom is sufficient", Detail: detail,
		})
	}

	return out
}

func fallbackReason(r string) string {
	if r == "" {
		return "no data directory was reported by the server"
	}
	return r
}
