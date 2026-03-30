Scan the codebase for common issues:

1. Run `go vet ./...` — check for suspicious constructs
2. Run `make lint` — golangci-lint analysis
3. Check for TODO/FIXME/HACK comments: `grep -rn "TODO\|FIXME\|HACK" internal/ cmd/`
4. Check for error returns that are not wrapped with context
5. Check for any `panic()` calls outside of main

Report findings organized by severity (errors > warnings > notes).
