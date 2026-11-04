# Contributing

Thanks for taking a look. This project has an unusually strict bar because a bug here can mean downtime or data loss at 2am for someone who cannot afford it.

## Ground rules

1. **Every check needs a positive test and a negative test.** A rule that fires on a healthy schema is worse than no rule at all.
2. **No capability claim without a versioned integration fixture.** If you say "`DROP COLUMN` is INSTANT on 8.0.29+", a test against a real server must prove it.
3. **The analyzer stays pure.** `internal/analyzer` takes a model and returns a plan. Database access belongs in `internal/introspect`.
4. **Fail closed.** Unknown operations are blocked, never guessed.
5. **Never widen what the tool will do** without also extending `docs/CHECKS.md` and the decision log.
6. **A frontend holds no policy.** `internal/cli` and `internal/wizard` render and prompt; they call `internal/core` for everything that decides, records, or executes. If a frontend needs to know whether a change is safe, that knowledge belongs in the analyzer.

## Layers

```
cmd/saksama      -> internal/cli -> internal/wizard -> internal/core
internal/app     -> connection and state directory wiring, used by both
internal/core    -> analyzer, introspect, engine, state, events, stream
```

Import rules, enforced by review:

- `core` never imports `cli` or `wizard`.
- `wizard` never imports `cli` (otherwise the dispatch is circular).
- A read-only question calls `core.Analyze`; only a decision anyone asked to keep calls `core.Prepare`. Advise and previews must not write sessions.

## Getting started

The full command reference is in [COMMANDS.md](COMMANDS.md).

```bash
git clone git@github.com:perogeremmer/saksama-core.git
cd saksama-core
make test              # unit tests, no database needed
make demo              # renders two plan cards
make test-integration  # spins MySQL 8.0 in docker, runs 20 tests
```

## Adding a check

1. Add the rule ID and logic in `internal/analyzer` (`checks.go` or `live.go` for anything needing a connection).
2. Document it in `docs/CHECKS.md`: ID, severity, SQL source, the MySQL error it prevents, and the remediation text.
3. Write the negative test first, prove it does not fire on a healthy schema.
4. Write the positive test with a fixture that actually triggers it.
5. If it needs live data, extend `internal/introspect` and add an integration test.

## Adding an engine

Implement `internal/engine.Engine`:

```go
Name() Name
Statements() ([]Statement, error)   // pure: same input, same output, no I/O
Preflight(ctx) error
Execute(ctx, onProgress) (Execution, error)
Verify(ctx) ([]VerifyResult, error)
```

Rules: never parse engine log output for progress (poll the database instead), never place a password in argv, and never bundle `pt-online-schema-change` (GPL-2) into a release artifact.

## Commit messages

Conventional Commits, imperative mood:

```
fix(native): omit LOCK clause for INSTANT DDL

MySQL rejects ALGORITHM=INSTANT together with LOCK (error 1221), so every
ADD COLUMN failed at execution while the plan reported it as safe.
```

## Pull requests

- One concern per PR.
- Include the output of `make test` and, if you touched execution, `make test-integration`.
- Update `README.md` and `ARCHITECTURE.md` when something moves from "not built" to "done".
