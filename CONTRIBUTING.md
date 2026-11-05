# Contributing

Thanks for taking a look. This project has an unusually strict bar because a bug here can mean downtime or data loss at 2am for someone who cannot afford it.

## Ground rules

1. **Every check needs a positive test and a negative test.** A rule that fires on a healthy schema is worse than no rule at all.
2. **No capability claim without a versioned integration fixture.** If you say "`DROP COLUMN` is INSTANT on 8.0.29+", a test against a real server must prove it. The same holds for host advice: if the tool prints a package command, a CI job runs it on that distribution.
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
make test-integration  # spins MySQL 8.0 in docker, runs the whole integration suite
```

Two things that surprise people the first time:

- **The copy-engine tests skip when the engines are missing.** `TestKilledCopy…`,
  the `pt-osc` cases and the flag matrix need `gh-ost` and
  `pt-online-schema-change` on `PATH`. Without them the suite is green and half of
  it never ran, so install both before you trust a local run:
  `saksama engine doctor` says what is missing, `saksama engine install gh-ost`
  and `… pt-osc` put the verified builds in place.
- **The slow tests are opt-in.** `SAKSAMA_VOLUME_TESTS=1 go test -tags=integration
  -run TestCopyWithConcurrentWrites ./test/integration/` builds 131k rows and writes
  to them while the copy runs. It takes minutes and runs nightly in CI, not per
  pull request.

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

## Releasing

A tag is the version: `git tag -a vX.Y.Z -F notes.txt && git push origin vX.Y.Z`
triggers `.github/workflows/release.yml`, which re-runs the tests, builds four
static binaries plus SBOMs, and publishes them.

Archives carry the binary and `LICENSE`, not the documentation: a README bundled into
a tarball is a snapshot of the tag date, so every correction would need a release.
Documents live in the repository at the tag.

Replacing a tag is allowed and supported: `release.mode: replace` in
`.goreleaser.yaml` means a second run overwrites the assets instead of failing every
upload with `422 already_exists`. That is for the case this repository has already
hit twice - an annotation that came out wrong, or a release that published nothing -
not for quietly changing what a version contains.

## What CI runs

| Job | What it proves |
|---|---|
| Unit tests | the analyzer, parsers, and graders, with the binary asserted **statically linked** (`file` and `ldd`), because "one file, any distribution" rests on it |
| Integration (MySQL 8.0, 8.4) | the whole suite against real servers |
| INSTANT boundary (8.0.28, 8.0.29) | the capability matrix against both sides of the boundary that matters |
| Claim checks | 5.6 plans with its caveat and blocks in production; MariaDB refused; 5.7 runs the full suite |
| pt-osc against a real binary | the Perl engine end to end, not a stub |
| Distro smoke (Fedora, Arch, Alpine) | the shipped binary runs there, and `engine doctor` names the package manager that distribution actually has |
| Engine flag matrix | every flag the wrappers emit exists in the real binaries |
| Release config | a snapshot builds, archives, checksums, and SBOMs, and the snapshot binary is static |
| Offline plan demo | three plans that must route, refuse, and explain without a database |

Nightly, not per pull request: `TestCopyWithConcurrentWritesLosesNothing` copies 131k
rows under continuous writes and asserts no committed write is lost.

## Pull requests

- One concern per PR.
- Include the output of `make test` and, if you touched execution, `make test-integration`
  with the engines installed, so the copy-engine tests actually ran instead of skipping.
- Update `README.md` and `ARCHITECTURE.md` when something moves from "not built" to "done".
