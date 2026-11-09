# What and why

Examine before you alter: state the problem this change fixes, and why it is
fixed this way. A change that could not be explained in one paragraph is
probably several changes.

Fixes # (issue)

## Type of change

- [ ] Bug fix - behaviour was wrong, or a guarantee was silently absent
- [ ] New feature
- [ ] Refactor - no operator-visible change
- [ ] Docs only

# Evidence

What proves the fix is real, not just plausible? Name the tests; a fix without
a test that fails before it and passes after is a claim, not a fix.

- [ ] Tests added (name them)
- [ ] `gofmt -l .` clean, `go vet ./...` clean
- [ ] `go test ./...` passes
- [ ] `go test -race ./internal/...` passes when goroutines were touched
- [ ] `make test-integration` passes when a claim about a live MySQL was touched

# Operator-visible change

What does a person running `saksama` type, see, or get back differently?
Include the exit code if one changed. If nothing changes, say "none".

# Honest limits

What did this deliberately not do, and why? Nothing is ever finished; say what
was left on the table so the next person does not rediscover it.
