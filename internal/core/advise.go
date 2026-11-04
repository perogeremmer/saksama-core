package core

import (
	"context"
	"fmt"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
	"github.com/perogeremmer/saksama-core/internal/introspect"
	"github.com/perogeremmer/saksama-core/internal/model"
)

// SchemaAdvice is the answer to "what is in this database, and what would it cost
// to change?". It is deliberately data, not text: the CLI renders it, the wizard
// renders it, and a web request serialises it.
type SchemaAdvice struct {
	Database string
	Server   model.Server
	// Tables are the assessments, largest first.
	Tables []analyzer.TableAssessment
	// Artifacts are leftovers already present in the schema.
	Artifacts []introspect.Artifact
	// Settings are the raw server variables, for the doctor.
	Settings map[string]string
}

// AdviseSchema reads a whole schema, or one table, and assesses every rebuild.
//
// No session is created and nothing is written. The operator is asking a
// question, and a question must not leave a trace in the state directory: a
// dashboard that opened ten tables would otherwise file ten fake sessions.
func (r *Runner) AdviseSchema(ctx context.Context, database, table string, binaries map[string]analyzer.EngineBinary) (SchemaAdvice, error) {
	if r.DB == nil {
		return SchemaAdvice{}, fmt.Errorf("no database connection")
	}
	if database == "" {
		return SchemaAdvice{}, fmt.Errorf("no database given")
	}

	collector := introspect.NewCollector(r.DB, database, table)
	srv, err := collector.Server(ctx)
	if err != nil {
		return SchemaAdvice{}, err
	}

	var tables []model.Table
	if table != "" {
		tbl, err := collector.Table(ctx)
		if err != nil {
			return SchemaAdvice{}, err
		}
		tables = append(tables, tbl)
	} else {
		tables, err = collector.SchemaTables(ctx, false)
		if err != nil {
			return SchemaAdvice{}, err
		}
	}

	settings, _ := collector.Settings(ctx)

	return SchemaAdvice{
		Database:  database,
		Server:    srv,
		Tables:    analyzer.AssessSchema(tables, srv, binaries, settings),
		Artifacts: introspect.FindArtifacts(tables),
		Settings:  settings,
	}, nil
}

// SettingChecks turns the observed server variables into the doctor's rows.
func (a SchemaAdvice) SettingChecks() []analyzer.SettingCheck {
	return analyzer.SettingChecks(analyzer.SettingsFromMap(a.Settings))
}
