# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Private GoReleaser + Homebrew release workflow for Baselayer fork
- Taskfile with release, dry-run, and homebrew test tasks
- Use name-set matching for unapplied migration detection
- Deferred schema cleanup to avoid downtime when applying multiple migrations
