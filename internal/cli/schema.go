package cli

// planJSONSchema documents the machine-readable plan contract.
//
// This is the "for app builders" surface described in docs/EVENTS.md: a stable
// schema another tool can code against. It is maintained by hand alongside
// internal/analyzer/analyze.go and is validated by a test.
const planJSONSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://saksama.dev/schema/plan-v1.json",
  "title": "Saksama plan",
  "type": "object",
  "required": ["contract_version", "generated_at", "target", "alter", "server", "schema", "schema_fingerprint", "operations", "recommendation", "checks", "duration", "disk", "recovery", "verification", "engines", "table_facts", "environment"],
  "additionalProperties": false,
  "properties": {
    "contract_version": { "const": 1 },
    "generated_at": { "type": "string", "format": "date-time" },
    "target": {
      "type": "object",
      "required": ["host", "database", "table", "production"],
      "additionalProperties": false,
      "properties": {
        "host": { "type": "string" },
        "database": { "type": "string" },
        "table": { "type": "string" },
        "production": { "type": "boolean" }
      }
    },
    "alter": { "type": "string" },
    "alter_normalized": { "type": "string" },
    "server": {
      "type": "object",
      "required": ["version", "vendor", "managed"],
      "additionalProperties": false,
      "properties": {
        "version": { "type": "string" },
        "vendor": { "type": "string" },
        "managed": { "type": "boolean" },
        "default_charset": { "type": "string" },
        "default_collation": { "type": "string" }
      }
    },
    "schema": {
      "type": "object",
      "required": ["engine", "row_format", "charset", "collation", "columns", "indexes", "outgoing_fk", "incoming_fk", "estimated_rows", "data_bytes", "index_bytes", "partitioned", "source"],
      "additionalProperties": false,
      "properties": {
        "engine": { "type": "string" },
        "row_format": { "type": "string" },
        "charset": { "type": "string" },
        "collation": { "type": "string" },
        "columns": { "type": "integer" },
        "indexes": { "type": "integer" },
        "outgoing_fk": { "type": "integer" },
        "incoming_fk": { "type": "integer" },
        "estimated_rows": { "type": "integer" },
        "data_bytes": { "type": "integer" },
        "index_bytes": { "type": "integer" },
        "partitioned": { "type": "boolean" },
        "source": { "enum": ["live", "ddl", "json"] }
      }
    },
    "schema_fingerprint": { "type": "string", "pattern": "^sha256:[0-9a-f]{64}$" },
    "operations": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["kind", "raw", "rule_id", "method", "reason"],
        "additionalProperties": false,
        "properties": {
          "kind": { "type": "string" },
          "raw": { "type": "string" },
          "rule_id": { "type": "string" },
          "method": { "enum": ["INSTANT", "INPLACE", "COPY", "UNSUPPORTED"] },
          "lock": { "type": "string" },
          "reason": { "type": "string" },
          "blocked": { "type": "boolean" }
        }
      }
    },
    "recommendation": {
      "type": "object",
      "required": ["engine", "fallbacks", "reasons", "blocked", "blockers", "warnings", "infos"],
      "additionalProperties": false,
      "properties": {
        "analyzer": { "enum": ["native", "gh-ost", "pt-osc", "none"] },
        "engine_source": { "type": "string" },
        "engine": { "enum": ["native", "gh-ost", "pt-osc", "none"] },
        "fallbacks": { "type": "array", "items": { "enum": ["native", "gh-ost", "pt-osc", "none"] } },
        "reasons": { "type": "array", "items": { "type": "string" } },
        "blocked": { "type": "boolean" },
        "blockers": { "type": "integer" },
        "warnings": { "type": "integer" },
        "infos": { "type": "integer" }
      }
    },
    "checks": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["id", "severity", "title"],
        "additionalProperties": false,
        "properties": {
          "id": { "type": "string" },
          "severity": { "enum": ["BLOCKER", "WARN", "INFO"] },
          "title": { "type": "string" },
          "detail": { "type": "string" },
          "remediation": { "type": "string" },
          "mysql_error": { "type": "integer" }
        }
      }
    },
    "duration": {
      "type": "object",
      "required": ["from_seconds", "to_seconds", "confidence", "basis"],
      "additionalProperties": false,
      "properties": {
        "from_seconds": { "type": "integer", "minimum": 0 },
        "to_seconds": { "type": "integer", "minimum": 0 },
        "confidence": { "enum": ["low", "medium", "high"] },
        "basis": { "type": "string" }
      }
    },
    "disk": {
      "type": "object",
      "required": ["required_bytes", "note"],
      "additionalProperties": false,
      "properties": {
        "required_bytes": { "type": "integer", "minimum": 0 },
        "note": { "type": "string" }
      }
    },
    "recovery": {
      "type": "object",
      "required": ["before_execution", "during_execution", "after_success", "hold_cutover"],
      "additionalProperties": false,
      "properties": {
        "before_execution": { "type": "string" },
        "during_execution": { "type": "string" },
        "after_success": { "type": "string" },
        "hold_cutover": { "type": "boolean" }
      }
    },
    "verification": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["kind", "name"],
        "additionalProperties": false,
        "properties": {
          "kind": { "enum": ["index_exists", "index_absent", "column_exists", "column_absent", "foreign_key_present"] },
          "name": { "type": "string" },
          "table": { "type": "string" }
        }
      }
    },
    "table_facts": {
      "description": "Observed schema facts the advice is based on.",
      "type": "array",
      "items": {
        "type": "object",
        "required": ["fact", "effect"],
        "additionalProperties": false,
        "properties": {
          "fact": { "type": "string" },
          "effect": { "type": "string" }
        }
      }
    },
    "environment": {
      "description": "Server settings the engines need, next to what the server reports.",
      "type": "array",
      "items": {
        "type": "object",
        "required": ["setting", "current", "required", "status", "needed_by"],
        "additionalProperties": false,
        "properties": {
          "setting": { "type": "string" },
          "current": { "type": "string" },
          "required": { "type": "string" },
          "status": { "enum": ["ok", "fail", "warn", "unknown"] },
          "needed_by": { "type": "array", "items": { "enum": ["native", "gh-ost", "pt-osc", "none"] } },
          "note": { "type": "string" }
        }
      }
    },
    "engines": {
      "description": "Suitability of every execution path for this specific change, including paths this build cannot run yet.",
      "type": "array",
      "items": {
        "type": "object",
        "required": ["engine", "verdict", "summary", "requirements", "executable"],
        "additionalProperties": false,
        "properties": {
          "engine": { "enum": ["native", "gh-ost", "pt-osc", "none"] },
          "verdict": { "enum": ["suitable", "unsuitable"] },
          "summary": { "type": "string" },
          "executable": { "type": "boolean" },
          "installed": { "type": "boolean" },
          "environment": {
            "type": "array",
            "items": {
              "type": "object",
              "required": ["name", "met", "detail"],
              "additionalProperties": false,
              "properties": {
                "name": { "type": "string" },
                "met": { "type": "boolean" },
                "detail": { "type": "string" }
              }
            }
          },
          "wrapped": { "type": "boolean" },
          "install": { "type": "string" },
          "command": { "type": "string" },
          "pros": { "type": "array", "items": { "type": "string" } },
          "cons": { "type": "array", "items": { "type": "string" } },
          "steps": { "type": "array", "items": { "type": "string" } },
          "evidence": {
            "type": "array",
            "items": {
              "type": "object",
              "required": ["fact", "effect"],
              "additionalProperties": false,
              "properties": {
                "fact": { "type": "string" },
                "effect": { "type": "string" }
              }
            }
          },
          "notes": { "type": "array", "items": { "type": "string" } },
          "requirements": {
            "type": "array",
            "items": {
              "type": "object",
              "required": ["name", "met", "detail"],
              "additionalProperties": false,
              "properties": {
                "name": { "type": "string" },
                "met": { "type": "boolean" },
                "detail": { "type": "string" }
              }
            }
          }
        }
      }
    },
    "notes": { "type": "array", "items": { "type": "string" } }
  }
}
`
