package analyzer

import (
	"strconv"
	"strings"
)

// ServerSettings is the subset of server configuration that decides whether an
// engine can run at all.
//
// It is separate from the engine verdict on purpose. "gh-ost cannot handle
// foreign keys" is a property of GitHub's tool; "binary logging is off" is a
// property of your server. Both stop a migration, and confusing them sends people
// looking in the wrong place.
type ServerSettings struct {
	Known bool
	Raw   map[string]string
}

// SettingsFromMap wraps a raw variable map. A nil or empty map means "not
// inspected", which every check reports as unknown rather than as a pass.
func SettingsFromMap(raw map[string]string) ServerSettings {
	return ServerSettings{Known: len(raw) > 0, Raw: raw}
}

// Get returns a variable, lower-cased.
func (s ServerSettings) Get(name string) (string, bool) {
	if s.Raw == nil {
		return "", false
	}
	v, ok := s.Raw[strings.ToLower(name)]
	return v, ok
}

// on reports whether a boolean-ish variable is enabled. MySQL reports these as
// ON/OFF, 1/0, or YES/NO depending on the variable.
func (s ServerSettings) on(name string) (value string, known, enabled bool) {
	v, ok := s.Get(name)
	if !ok || v == "" {
		return "", false, false
	}
	switch strings.ToUpper(v) {
	case "ON", "1", "YES", "TRUE":
		return v, true, true
	}
	return v, true, false
}

// EnvStatus is the outcome of one setting check.
type EnvStatus string

const (
	EnvOK      EnvStatus = "ok"
	EnvFail    EnvStatus = "fail"
	EnvWarn    EnvStatus = "warn"
	EnvUnknown EnvStatus = "unknown"
)

// SettingCheck is one row of the doctor report.
type SettingCheck struct {
	Setting  string    `json:"setting"`
	Current  string    `json:"current"`
	Required string    `json:"required"`
	Status   EnvStatus `json:"status"`
	NeededBy []Engine  `json:"needed_by"`
	Note     string    `json:"note,omitempty"`
}

// EngineEnvironment lists the server prerequisites for an engine, so a plan can
// say "gh-ost fits this table, but binary logging is off".
func EngineEnvironment(settings ServerSettings, engine Engine) []Requirement {
	var out []Requirement

	add := func(name string, met bool, detail string) {
		out = append(out, Requirement{Name: name, Met: met, Detail: detail})
	}

	writable := func() (bool, string) {
		if ro, known, on := settings.on("read_only"); known && on {
			return false, "read_only is " + ro
		}
		if ro, known, on := settings.on("super_read_only"); known && on {
			return false, "super_read_only is " + ro
		}
		if !settings.Known {
			return true, "not checked (no live connection)"
		}
		return true, "server accepts writes"
	}

	switch engine {
	case EngineGhOst:
		logBin, known, on := settings.on("log_bin")
		switch {
		case !settings.Known:
			add("binary logging enabled", true, "not checked")
		case !known:
			add("binary logging enabled", false, "log_bin is not reported by this server")
		default:
			add("binary logging enabled", on, "log_bin is "+logBin)
		}

		format, known := settings.Get("binlog_format")
		switch {
		case !settings.Known:
			add("binlog_format is ROW", true, "not checked")
		case !known:
			add("binlog_format is ROW", false, "binlog_format is not reported")
		default:
			add("binlog_format is ROW", strings.EqualFold(format, "ROW"), "binlog_format is "+format)
		}

		rowImage, known := settings.Get("binlog_row_image")
		switch {
		case !settings.Known:
			add("binlog_row_image is FULL", true, "not checked")
		case !known:
			add("binlog_row_image is FULL", false, "binlog_row_image is not reported")
		default:
			add("binlog_row_image is FULL", strings.EqualFold(rowImage, "FULL"), "binlog_row_image is "+rowImage)
		}

		if id, known := settings.Get("server_id"); known {
			n, _ := strconv.Atoi(id)
			add("server_id is set", n != 0, "server_id is "+id)
		} else if settings.Known {
			add("server_id is set", false, "server_id is not reported")
		}

		ok, detail := writable()
		add("server accepts writes", ok, detail)

		if ps, known, on := settings.on("performance_schema"); known && !on {
			add("performance_schema enabled", false, "performance_schema is "+ps+"; metadata lock detection will be blind")
		}

	case EnginePtOSC:
		ok, detail := writable()
		add("server accepts writes", ok, detail)

		isolation, known := settings.Get("transaction_isolation")
		if !settings.Known {
			add("REPEATABLE READ or lower", true, "not checked")
		} else if known {
			add("REPEATABLE READ or lower", !strings.EqualFold(isolation, "SERIALIZABLE"),
				"transaction_isolation is "+isolation)
		}

		if mode, known := settings.Get("log_bin_trust_function_creators"); known {
			_ = mode // trigger creation under binlog needs SUPER, not this flag
		}

	case EngineNative:
		ok, detail := writable()
		add("server accepts writes", ok, detail)

		if ps, known, on := settings.on("performance_schema"); known && !on {
			out = append(out, Requirement{Name: "performance_schema enabled", Met: false,
				Detail: "performance_schema is " + ps + "; the metadata lock check cannot see other sessions"})
		}
	}

	return out
}

// SettingChecks builds the doctor report: every setting that any engine cares
// about, with the value the server actually reports.
func SettingChecks(settings ServerSettings) []SettingCheck {
	type spec struct {
		name     string
		required string
		neededBy []Engine
		note     string
	}

	specs := []spec{
		{"log_bin", "ON", []Engine{EngineGhOst}, "gh-ost follows writes through it"},
		{"binlog_format", "ROW", []Engine{EngineGhOst}, "statement format loses row data"},
		{"binlog_row_image", "FULL", []Engine{EngineGhOst}, "MINIMAL omits needed columns"},
		{"server_id", "non-zero", []Engine{EngineGhOst}, "gh-ost refuses server_id=0"},
		{"read_only", "OFF", []Engine{EngineNative, EngineGhOst, EnginePtOSC}, "read-only rejects every write"},
		{"super_read_only", "OFF", []Engine{EngineNative, EngineGhOst, EnginePtOSC}, "overrides read_only"},
		{"performance_schema", "ON", []Engine{EngineNative, EngineGhOst}, "needed to see other sessions locks"},
		{"transaction_isolation", "REPEATABLE-READ or lower", []Engine{EnginePtOSC}, "SERIALIZABLE widens locking"},
		{"innodb_file_per_table", "ON", []Engine{}, "OFF never frees a dropped table"},
		{"sql_require_primary_key", "OFF", []Engine{EngineNative, EngineGhOst, EnginePtOSC}, "ON rejects DDL on keyless tables"},
		{"lower_case_table_names", "any (be consistent)", []Engine{}, "host and server must agree"},
		{"gtid_mode", "either", []Engine{}, "neither engine requires it"},
	}

	var out []SettingCheck
	for _, sp := range specs {
		c := SettingCheck{Setting: sp.name, Required: sp.required, NeededBy: sp.neededBy, Note: sp.note}
		value, known := settings.Get(sp.name)

		if !settings.Known {
			c.Current = "(not inspected)"
			c.Status = EnvUnknown
			out = append(out, c)
			continue
		}
		if !known {
			c.Current = "(not reported)"
			// A variable that does not exist on this server is not a failure for
			// settings we only care about when present.
			if sp.name == "sql_require_primary_key" || sp.name == "gtid_mode" || sp.name == "innodb_file_per_table" {
				c.Status = EnvOK
			} else {
				c.Status = EnvUnknown
			}
			out = append(out, c)
			continue
		}

		c.Current = value
		c.Status = gradeSetting(sp.name, value, len(sp.neededBy) > 0)
		out = append(out, c)
	}

	return out
}

// gradeSetting decides ok / warn / fail for one value.
func gradeSetting(name, value string, requiredByAnEngine bool) EnvStatus {
	upper := strings.ToUpper(strings.TrimSpace(value))
	failIfNeeded := func(bad bool) EnvStatus {
		if !bad {
			return EnvOK
		}
		if requiredByAnEngine {
			return EnvFail
		}
		return EnvWarn
	}

	switch name {
	case "log_bin", "performance_schema", "innodb_file_per_table":
		return failIfNeeded(upper != "ON" && upper != "1" && upper != "YES")
	case "binlog_format":
		return failIfNeeded(upper != "ROW")
	case "binlog_row_image":
		return failIfNeeded(upper != "FULL")
	case "server_id":
		n, err := strconv.Atoi(upper)
		return failIfNeeded(err != nil || n == 0)
	case "read_only", "super_read_only":
		return failIfNeeded(upper == "ON" || upper == "1")
	case "transaction_isolation":
		return failIfNeeded(upper == "SERIALIZABLE")
	case "sql_require_primary_key":
		return failIfNeeded(upper == "ON" || upper == "1")
	}
	return EnvOK
}

// EnvironmentSummary counts the outcomes, for the one-line verdict.
func EnvironmentSummary(checks []SettingCheck) (fails, warns, unknown int) {
	for _, c := range checks {
		switch c.Status {
		case EnvFail:
			fails++
		case EnvWarn:
			warns++
		case EnvUnknown:
			unknown++
		}
	}
	return
}

// FixHints returns what to change, in the order a person would do it.
func FixHints(checks []SettingCheck) []string {
	var out []string
	for _, c := range checks {
		if c.Status != EnvFail {
			continue
		}
		switch c.Setting {
		case "log_bin":
			out = append(out, "log_bin: enable binary logging in your my.cnf (log_bin=ON) and restart the server")
		case "binlog_format":
			out = append(out, "binlog_format=ROW: set it in my.cnf, or SET GLOBAL binlog_format='ROW' if your privileges allow")
		case "binlog_row_image":
			out = append(out, "binlog_row_image=FULL: needed by gh-ost to reconstruct full rows")
		case "server_id":
			out = append(out, "server_id: set any non-zero value (server_id=1 is fine on a single server)")
		case "read_only", "super_read_only":
			out = append(out, c.Setting+": the server is read-only; point Saksama at the writable primary")
		case "sql_require_primary_key":
			out = append(out, "sql_require_primary_key=ON: DDL on keyless tables is refused; add a key first or relax the setting for this session")
		case "performance_schema":
			out = append(out, "performance_schema: enable it so metadata lock detection is not blind")
		}
	}
	return out
}
