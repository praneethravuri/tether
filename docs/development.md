# Development

Tether is a Go CLI plus an auto-started local daemon. The daemon exposes a
Unix socket and persists workspace-scoped messages and claims in SQLite.
Changes to command behavior must keep the Cobra help, README, bundled agent
skill, and tests aligned.

## Prerequisites

- The Go version declared in [`go.mod`](../go.mod)
- `golangci-lint`
- `ShellCheck`
- GoReleaser v2.17.0

## Local development

```sh
make build       # builds ./tether with a version derived from git
make test        # race-enabled package tests
make lint        # golangci-lint
make fmt         # formats Go files in place
make vet         # static analysis
make tidy        # updates module metadata
```

Run the binary from another terminal while developing:

```sh
./tether start
```

Daemon-facing client commands auto-start the daemon when needed, so normally
you can run `./tether register <name>`, `./tether send ...`, and the other
commands directly. `./tether doctor` is the intentional exception: it only
reports whether a daemon is reachable. For an isolated local run, set
`TETHER_SOCK`, `TETHER_DB`, and `TETHER_WORKSPACE` to test-specific values.
The directory containing an overridden `TETHER_SOCK` must not be writable by
group or other users; use a fresh directory from `mktemp -d` (normally `0700`),
not `/tmp` itself.

## Required verification

Before opening a pull request, run the release checks:

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

The snapshot must also pass the installer end-to-end check: serve its four
archives and `checksums.txt` over trusted HTTPS, run `docs/install.sh` with
`TETHER_BASE_URL` and a temporary `TETHER_INSTALL_DIR`, then run the installed
binary's `version` command. The `install-e2e` GitHub Actions job is the
canonical implementation of that check.

`gofmt -s -l .` must produce no paths. The command above uses `test -z` so a
non-empty result fails immediately without rewriting files.

## Continuous integration

The pull-request workflow enforces module tidiness, builds and race-enabled
tests on Linux and macOS, a formatting check, `golangci-lint`, ShellCheck for
the installer, and the GoReleaser snapshot installer E2E. Scheduled security
scanning runs the pinned Go vulnerability scanner; Dependabot monitors Go
modules and GitHub Actions.

`go vet ./...` remains a required local and release verification, even though
it is not a separate CI job.

## Documentation and releases

The command reference is maintained with the command code rather than
generated. When a command, positional argument, flag, output shape, or exit
status changes, update all of these in the same change:

- [`README.md`](../README.md)
- [`skills/tether/SKILL.md`](../skills/tether/SKILL.md)
- Cobra help and examples under `cmd/tether/`
- tests that exercise the changed command

Follow [`docs/releasing.md`](releasing.md) for the complete release procedure.
