# Changelog

All notable changes to this project are documented here. Format loosely
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Changed

- Renamed the project from `intern` to `tether`: Go module path, binary,
  CLI verb, GitHub repo, and bundled agent skill. This is a breaking change
  with no compatibility shim — `INTERN_SOCK`, `INTERN_DB`,
  `INTERN_WORKSPACE`, `INTERN_SESSION_ID`, and `INTERN_VERSION` become
  `TETHER_SOCK`, `TETHER_DB`, `TETHER_WORKSPACE`, `TETHER_SESSION_ID`, and
  `TETHER_VERSION`; the state directory moves from `~/.intern` to
  `~/.tether`. Existing `~/.intern/intern.db` installations are not migrated.

## [0.3.2] - 2026-08-01

### Fixed

- Kept named plain-shell agents authenticated across `send`, `wait`, and
  `inbox` after `register`.
- Made `ls --all` and `claims --all` query every workspace even when
  `--workspace` is also present.
- Prevented concurrent stale-lock recovery, socket startup over regular files,
  unsafe rename reclamation, false-positive doctor status, and sub-millisecond
  waits that could become a one-minute daemon request.
- Corrected command help, README, and bundled-agent guidance to match the
  workspace, auto-start, all-workspace, and exit-code contracts.

## [0.3.1] - 2026-08-01

### Fixed

- Corrected command help, README, bundled agent guidance, and release notes to
  describe only the retained daemon and SQLite command surface.
- Removed inactive inbox output controls and documentation for unavailable
  commands and flags.
- Restored focused JSON command wiring coverage and a real-process handoff
  check for register, send, wait, and inbox.

### Changed

- Reduced CI to the build, formatting, race-test, lint, installer, dependency,
  and vulnerability checks that protect the supported release flow.
- Added a manual release procedure with GoReleaser snapshot, checksum, and
  installed-binary verification.

## [0.3.0] - 2026-08-01

### Removed

- Removed Claude Code hooks (`intern hooks install`, `run-stop`, `run-session-start`). `intern wait` is the universal push mechanism.
- Removed `intern demo` and `intern top`.
- Removed the optional JSON-output switch; daemon-facing command results are JSON by default.
- Removed `--doing` flag and `last_note` column.
- Removed protocol version guard (`v` field on every request).
- Reset schema version to v1 (collapsed 5 migration steps into one canonical schema).

### Changed

- Daemon-facing command results output JSON to stdout; `start` and `version` keep their text output.
- Bare `intern` (no args) returns workspace listing as JSON.

### Simplified

- ~3,000 lines of code removed (hooks, migrations, human-readable formatting, dead commands).
- Schema migrations collapsed: fresh databases use v1 directly.

## [0.2.0] - 2026-07-31

### Changed

- Renamed the CLI, Go module, local state directory, installer variables, and release archives to `intern`.
- Kept the existing command verbs and local daemon protocol stable.
- Reworked the README and bundled agent skill for the `intern` workflow.

### Added

- Published v0.2.0 manually with GoReleaser; future releases use explicit version tags.
