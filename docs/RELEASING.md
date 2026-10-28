# Releasing

Releases are automated with [release-please](https://github.com/googleapis/release-please).
You don't hand-edit version numbers or the changelog. You write
[Conventional Commits](https://www.conventionalcommits.org/), and the tooling
does the rest.

## How it works

```
commit to main (feat:/fix:/...) ──► release-please.yml opens/updates a "Release PR"
                                          │
                              merge the Release PR
                                          │
                     tag v0.2.0 is created ──► release.yml builds + publishes
```

1. **You commit** to `main` (via PRs) using Conventional Commit messages.
2. **`release-please.yml`** reads the commits since the last tag, works out the
   next semver bump, and keeps a **Release PR** open as a draft. That PR bumps
   `plugin.json`'s `version` and updates `CHANGELOG.md`.
3. **You merge the Release PR** when you're ready to ship. That creates the
   `v*` git tag.
4. **`release-please.yml`** then calls **`release.yml`** for that tag (a tag
   you push by hand triggers `release.yml` directly). Its `build` job refuses a tag that is
   not on `main`, then runs the full security-gated `make release` (Grype,
   govulncheck, CodeQL) with no
   signing key. Its `publish` job runs in the `release` environment, runs no
   repository code, signs (if configured) and checksums the bundle, and
   publishes a GitHub Release with the bundle, checksum, SBOMs, CodeQL SARIF,
   and signature.

Nothing releases until you merge the Release PR. Merging normal PRs to `main`
only updates the pending release, it doesn't ship.

## Commit message format

The prefix on the commit **subject** drives the version bump:

| Prefix | Example | Bump | Changelog section |
|--------|---------|------|-------------------|
| `feat:` | `feat: add channel export command` | minor (`0.1.0 → 0.2.0`) | Features |
| `fix:` | `fix: handle empty webhook payload` | patch (`0.1.0 → 0.1.1`) | Bug Fixes |
| `perf:` | `perf: cache config lookups` | patch | Performance |
| `deps:` | `deps(go): bump gorilla/mux` | patch | Dependencies |
| `feat!:` / `BREAKING CHANGE:` | `feat!: drop MM 10 support` | minor while pre-1.0 (`0.1.0 → 0.2.0`), major after | Features + ⚠ Breaking |
| `docs:` `chore:` `test:` `refactor:` `style:` `build:` `ci:` | `chore: tidy imports` | none | hidden |

Rules of thumb:

- Subject lines with **no** conventional prefix are left out of the changelog, so
  use `feat:`/`fix:` deliberately.
- Squash-merging PRs? The **PR title** becomes the commit subject, so title PRs
  with a conventional prefix.
- Pre-1.0 (`0.x`), breaking changes bump the **minor**, not the major, per
  semver's initial-development clause (`bump-minor-pre-major` in
  `.release-please-config.json`). Cut `1.0.0` deliberately with a
  `Release-As: 1.0.0` commit footer.

## Cutting a release (normal path)

1. Land your feature/fix PRs to `main` with conventional commit titles.
2. Find the open **"chore(main): release X.Y.Z"** PR that release-please keeps
   updated. Review its `CHANGELOG.md` and `plugin.json` diff.
3. Mark it ready for review, then merge it. The tag and GitHub Release are created automatically.

## Cutting a release (manual fallback)

If you need to ship without release-please (hotfix, tooling outage):

```sh
# 1. Bump the version in plugin.json and add a CHANGELOG.md entry, then commit.
git commit -am "chore: release v0.2.0"
git push origin main

# 2. Tag and push. This fires release.yml.
git tag -a v0.2.0 -m "release v0.2.0"
git push origin v0.2.0
```

`make release-check` (run as the first step of `make release`) fails the build
if the working tree is dirty or `CHANGELOG.md` is missing, so commit everything
first.

## What ships in a release

`release.yml` runs `make release`, which produces and attaches:

- `<plugin-id>-<version>.tar.gz`: the plugin bundle (with `sbom/` and
  `security/` SARIF embedded inside it)
- `.sha256` checksum sidecar
- `.sig` detached GPG signature (only if a signing key is configured; see
  [SECURITY.md](../SECURITY.md))
- `server-sbom.json` / `webapp-sbom.json`: CycloneDX SBOMs
- `codeql-go.sarif` / `codeql-js.sarif`: static-analysis results

## Pre-releases

A tag containing `-rc`, `-beta` or `-alpha` (for example `v0.3.0-rc.1`) is
published as a GitHub pre-release. The tag must still point at a commit on
`main`. Push one with the manual fallback above.
