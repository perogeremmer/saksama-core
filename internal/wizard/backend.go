package wizard

import (
	"context"
	"database/sql"

	"github.com/perogeremmer/saksama-core/internal/analyzer"
	"github.com/perogeremmer/saksama-core/internal/app"
	"github.com/perogeremmer/saksama-core/internal/core"
	"github.com/perogeremmer/saksama-core/internal/cred"
	"github.com/perogeremmer/saksama-core/internal/enginesetup"
	"github.com/perogeremmer/saksama-core/internal/introspect"
	"github.com/perogeremmer/saksama-core/internal/state"
)

// Backend is everything the wizard is allowed to do.
//
// Every method is a core function or a thin composition of one. The wizard has no
// SQL, no analyzer, and no policy: if a method here ever needs to decide whether a
// change is safe, that decision belongs in core and the wizard should only show
// the result. The interface also makes the whole menu testable with a fake.
type Backend interface {
	Connect(conn cred.Conn) error
	Close() error

	Schema(ctx context.Context, database, table string) (core.SchemaAdvice, error)
	Doctor(ctx context.Context) ([]enginesetup.Checklist, error)

	Analyze(ctx context.Context, req core.Request) (*core.Prepared, error)
	Prepare(ctx context.Context, req core.Request) (*core.Prepared, error)
	Apply(ctx context.Context, req core.Request) (*state.Session, error)
	ApplyFromSession(ctx context.Context, id string, req core.Request) (*state.Session, error)

	Cancel(ctx context.Context, id string, force bool) (core.CancelResult, error)
	Retire(ctx context.Context, id string) (core.RetireResult, error)
	Cleanup(ctx context.Context, id string, dryRun bool) (core.CleanupResult, error)
	ReleaseCutover(ctx context.Context, id string) (*state.Session, error)
	Reconcile(ctx context.Context) ([]core.Finding, error)

	Binaries() map[string]analyzer.EngineBinary

	// Engine choices are decisions, not actions: they write to the state
	// directory and nothing else, which is why the menu may record one.
	EngineChoices(ctx context.Context) ([]state.EngineChoice, error)
	SetEngine(engine, database, table string, global bool, note string) (state.EngineChoice, error)
	ClearEngine(database, table string, global bool) (state.EngineChoice, error)
}

// live is the real backend: one connection, one runner, one state directory.
type live struct {
	store    *state.Store
	binaries map[string]analyzer.EngineBinary

	db     *sql.DB
	runner *core.Runner
}

func newLive(store *state.Store, binaries map[string]analyzer.EngineBinary) *live {
	return &live{store: store, binaries: binaries}
}

func (l *live) Connect(c cred.Conn) error {
	// Reconnecting must not leak the previous handle: a wizard session can switch
	// servers several times while an operator works out which host is which.
	if l.db != nil {
		_ = l.db.Close()
		l.db = nil
		l.runner = nil
	}
	db, err := app.Connect(c)
	if err != nil {
		return err
	}
	l.db = db
	l.runner = core.New(db, l.store)
	return nil
}

func (l *live) Close() error {
	if l.db == nil {
		return nil
	}
	err := l.db.Close()
	l.db = nil
	l.runner = nil
	return err
}

func (l *live) Schema(ctx context.Context, database, table string) (core.SchemaAdvice, error) {
	return l.runner.AdviseSchema(ctx, database, table, l.binaries)
}

func (l *live) Doctor(ctx context.Context) ([]enginesetup.Checklist, error) {
	settings := analyzer.ServerSettings{}
	live := false
	if l.db != nil {
		raw, err := introspect.NewCollector(l.db, "", "").Settings(ctx)
		if err != nil {
			return nil, err
		}
		settings = analyzer.SettingsFromMap(raw)
		live = true
	}
	lists := make([]enginesetup.Checklist, 0, 2)
	for _, name := range []string{enginesetup.GhOst, enginesetup.PtOSC} {
		lists = append(lists, enginesetup.Check(name, l.binaries, settings, live))
	}
	return lists, nil
}

func (l *live) Analyze(ctx context.Context, req core.Request) (*core.Prepared, error) {
	return l.runner.Analyze(ctx, req)
}

func (l *live) Prepare(ctx context.Context, req core.Request) (*core.Prepared, error) {
	return l.runner.Prepare(ctx, req)
}

func (l *live) Apply(ctx context.Context, req core.Request) (*state.Session, error) {
	return l.runner.Apply(ctx, req)
}

func (l *live) ApplyFromSession(ctx context.Context, id string, req core.Request) (*state.Session, error) {
	return l.runner.ApplyFromSession(ctx, id, req)
}

func (l *live) Cancel(ctx context.Context, id string, force bool) (core.CancelResult, error) {
	return l.runner.Cancel(ctx, id, force)
}

func (l *live) Retire(ctx context.Context, id string) (core.RetireResult, error) {
	return l.runner.Retire(ctx, id)
}

func (l *live) Cleanup(ctx context.Context, id string, dryRun bool) (core.CleanupResult, error) {
	return l.runner.Cleanup(ctx, id, dryRun)
}

func (l *live) ReleaseCutover(ctx context.Context, id string) (*state.Session, error) {
	return l.runner.ReleaseCutover(ctx, id)
}

func (l *live) Reconcile(ctx context.Context) ([]core.Finding, error) {
	return l.runner.Reconcile(ctx)
}

func (l *live) Binaries() map[string]analyzer.EngineBinary { return l.binaries }

func (l *live) EngineChoices(ctx context.Context) ([]state.EngineChoice, error) {
	return l.store.ListEngineChoices()
}

func (l *live) SetEngine(engine, database, table string, global bool, note string) (state.EngineChoice, error) {
	choice, err := state.NewChoice(engine, global, database, table)
	if err != nil {
		return state.EngineChoice{}, err
	}
	choice.Actor = currentActor()
	choice.Note = note
	if err := l.store.SetEngineChoice(choice); err != nil {
		return state.EngineChoice{}, err
	}
	return choice, nil
}

func (l *live) ClearEngine(database, table string, global bool) (state.EngineChoice, error) {
	choice, err := state.NewChoice("native", global, database, table)
	if err != nil {
		return state.EngineChoice{}, err
	}
	if _, err := l.store.ClearEngineChoice(choice); err != nil {
		return state.EngineChoice{}, err
	}
	return choice, nil
}
