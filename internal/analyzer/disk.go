package analyzer

import (
	"fmt"
	"strings"
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

// diskChecks evaluates D6: is there room to run this, and is the volume healthy.
//
// It runs for every plan, not only for rebuilds: an INSTANT change writes almost
// nothing, but a volume with 2 percent free is a problem regardless, and a
// migration is exactly the wrong time to discover it.
func diskChecks(in Input, needsCopy bool) []Check {
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
		remediation := "pass --free-disk and --disk-total, or run Saksama on the database host so the filesystem can be read"
		if !in.ForExecution {
			// Planning is useful without a measured disk; executing is not.
			return []Check{{
				ID: chkDiskHeadroom, Severity: SeverityWarn,
				Title:       "free disk space could not be measured",
				Detail:      detail,
				Remediation: remediation,
			}}
		}
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
			Remediation: "free space or move to a larger volume; a copy engine builds a second table before swapping",
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
				Remediation: "free space first; binlogs, undo, and application writes keep growing during a migration",
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

var _ = strings.TrimSpace
