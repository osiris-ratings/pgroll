# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.16.1-baselayer.6] - 2026-05-09

## [0.16.1-baselayer.5] - 2026-05-08

### Added
- Debian package distribution: GoReleaser now builds `linux/amd64` and `linux/arm64` `.deb` artifacts and attaches them to the GitHub release alongside the existing tarballs.
- `scripts/install-debian.sh` bootstrap script for first-time install on a Debian/Ubuntu VM (authenticates to the private release with a GitHub PAT).
- `pgroll-update` helper shipped inside the `.deb` (installed at `/usr/local/bin/pgroll-update`) for in-place upgrades — no more `scp`.
- Bash and zsh completions installed by the `.deb`.
- `task release:test:deb` — local end-to-end validation of the package via Docker.

## [0.16.1-baselayer.4] - 2026-05-07

## [0.16.1-baselayer.3] - 2026-05-07

## [0.16.1-baselayer.2] - 2026-05-07

## [0.16.1-baselayer.1] - 2026-05-05

### Added
- Private GoReleaser + Homebrew release workflow for Baselayer fork
- Taskfile with release, dry-run, and homebrew test tasks
- Use name-set matching for unapplied migration detection
- Deferred schema cleanup to avoid downtime when applying multiple migrations
