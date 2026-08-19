# Contributing

Tether is a local daemon and SQLite-backed coordination tool for coding
agents. Keep changes small, preserve the CLI's JSON-first contract, and avoid
adding commands, flags, or persistent state without a concrete workflow that
needs them.

## Build and test

```sh
make build
make test
make lint
make fmt
make vet
```

Before opening a pull request, run the full release verification:

```sh
go mod tidy -diff
go build ./...
go vet ./...
test -z "$(gofmt -s -l .)"
go test -race -count=1 ./...
golangci-lint run
shellcheck docs/install.sh
goreleaser check
goreleaser release --snapshot --clean
```

Also ensure the snapshot installer E2E passes: it must install a snapshot from
a trusted HTTPS endpoint, verify its checksum, and run the installed binary's
`version` command. The repository's `install-e2e` workflow performs that
check.

## Before you open a PR

- Add or update focused tests for the retained behavior you change. The test
  suite uses the Go standard library; do not add a test framework casually.
- Keep command help, [`README.md`](README.md), and
  [`skills/tether/SKILL.md`](skills/tether/SKILL.md) in sync with the actual
  Cobra command surface.
- Include a real CLI transcript for behavior changes. JSON output should make
  the request and result easy to inspect.
- Keep comments brief and explain only non-obvious intent.
- Do not add author or co-author trailers for tools or agents to commits.

CI checks module tidiness, Linux and macOS builds and race tests, formatting,
lint, the installer, and release snapshots. Scheduled vulnerability scanning
and Dependabot cover dependency hygiene.

## Reporting a bug

Open an issue with the output of `tether version`, your OS and architecture,
and the smallest command sequence that reproduces the problem. Include
`tether doctor` output when the issue involves the daemon or local state.
For a suspected security issue, contact the maintainer privately instead of
opening a public issue.
