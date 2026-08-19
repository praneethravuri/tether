# Releasing tether

Tether releases are prepared manually from a green, merged `main` branch. The
annotated release tag is the version source: GoReleaser injects it into the
binary, so there is no source-file version to edit. Never move an existing
release tag.

Set the version once for the whole procedure. For the next release, use
`v0.3.2`:

```sh
VERSION=v0.3.2
RELEASE_BRANCH="codex/release-${VERSION#v}"
test -z "$(git tag -l "$VERSION")"
```

## 1. Prepare and verify a release branch

Start from up-to-date `main` with no unrelated changes:

```sh
git fetch origin --tags
git switch main
git pull --ff-only origin main
git switch -c "$RELEASE_BRANCH"
```

Update `CHANGELOG.md`, release-version examples, and this procedure when the
release changes them. Keep any root `TODO.md` as a local working checklist;
do not stage or commit it. Before each commit, use the available code-review
workflow, apply valid simplifications, then rerun the checks affected by the
change. Commits use the configured human author only and contain no AI or
co-author trailers.

Run every local release gate before opening the pull request:

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

Also run the snapshot installer E2E. Serve its four archives and
`checksums.txt` over trusted HTTPS, use `TETHER_BASE_URL` and a temporary
`TETHER_INSTALL_DIR` with `docs/install.sh`, then run the installed binary’s
`version` command. The `install-e2e` job in [Go CI](../.github/workflows/go.yml)
is the canonical, executable form of that check.

Push the branch, open a pull request into `main`, and wait for every required
Go CI and Security check to pass:

```sh
git push -u origin "$RELEASE_BRANCH"
gh pr create --base main --head "$RELEASE_BRANCH" --title "release: $VERSION" --fill
gh pr checks --watch
```

## 2. Verify merged main

After the pull request is merged, verify the exact merge commit again:

```sh
git fetch origin --tags
git switch main
git pull --ff-only origin main
git merge-base --is-ancestor "$RELEASE_BRANCH" origin/main
```

Re-run the local release gates above, including the GoReleaser snapshot and
installer E2E. Confirm Go CI and Security are green for that exact commit:

```sh
COMMIT=$(git rev-parse HEAD)
gh run list --commit "$COMMIT" --workflow "Go CI"
gh run list --commit "$COMMIT" --workflow "Security"
gh run watch <run-id> --exit-status
```

## 3. Tag and publish

Create and push an annotated tag from the verified merge commit:

```sh
git tag -a "$VERSION" -m "Release $VERSION"
git push origin "$VERSION"
test "$(git cat-file -t "$VERSION")" = tag
test "$(git rev-list -n 1 "$VERSION")" = "$(git rev-parse origin/main)"
```

Publish the GitHub release with the pinned GoReleaser version from the
repository. `GITHUB_TOKEN` must be allowed to create releases in this
repository:

```sh
GITHUB_TOKEN="$(gh auth token)" goreleaser release --clean
```

This creates four archives—Linux and macOS for `amd64` and `arm64`—and
`checksums.txt`. The manual flow intentionally has no signing or SBOM tool
dependency; checksums protect transfer integrity, not an independently signed
provenance claim.

## 4. Verify public artifacts and installer

Download the published assets to a disposable directory, validate every
checksum, then install the tagged release:

```sh
RELEASE_DIR=$(mktemp -d)
gh release download "$VERSION" --repo praneethravuri/tether \
  --pattern 'tether_*.tar.gz' --pattern checksums.txt --dir "$RELEASE_DIR"

(
  cd "$RELEASE_DIR"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum -c checksums.txt
  else
    shasum -a 256 -c checksums.txt
  fi
)

INSTALL_DIR="$RELEASE_DIR/install"
TETHER_VERSION="$VERSION" TETHER_INSTALL_DIR="$INSTALL_DIR" sh docs/install.sh
test "$("$INSTALL_DIR/tether" version)" = "$VERSION"
```

All four checksums must validate and the installed binary must print exactly
the tag value.

## 5. Clean up the merged release branch

Keep `main` and `origin/main`. Delete only the merged release branch after
the tag and public-artifact verification succeed:

```sh
if git ls-remote --exit-code --heads origin "$RELEASE_BRANCH" >/dev/null; then
  git push origin --delete "$RELEASE_BRANCH"
fi
git branch -d "$RELEASE_BRANCH"
git switch main
```
