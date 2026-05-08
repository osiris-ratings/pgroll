# Installation

## Binaries

Binaries are available for Linux, macOS & Windows on our [Releases](https://github.com/xataio/pgroll/releases) page.

## From source

To install `pgroll` from source, run the following command:

```sh
go install github.com/xataio/pgroll@latest
```

Note: requires [Go 1.24](https://golang.org/doc/install) or later.

## From package manager - Homebrew

To install `pgroll` with homebrew, run the following command:

```sh
# macOS or Linux
brew tap xataio/pgroll
brew install pgroll
```

## From .deb (Debian / Ubuntu — Baselayer fork)

The Baselayer fork ships a `.deb` for `linux/amd64` and `linux/arm64` with each
release at [osiris-ratings/pgroll](https://github.com/osiris-ratings/pgroll/releases).
The package installs the binary at `/usr/bin/pgroll`, bash and zsh completions,
and a `pgroll-update` helper for upgrading in place.

Because the release repo is private, both first-time install and upgrades
authenticate to the GitHub API with a fine-grained Personal Access Token
scoped to `osiris-ratings/pgroll` with **Contents: Read**.

### One-time setup per VM

1. Place the token where the install scripts can find it:

   ```sh
   sudo install -d -m 0700 /etc/pgroll
   echo 'github_pat_...' | sudo tee /etc/pgroll/release-token >/dev/null
   sudo chmod 0600 /etc/pgroll/release-token
   ```

   (Or, for one-off installs, `export PGROLL_RELEASE_TOKEN=...` instead.)

2. Copy the bootstrap script to the VM and run it:

   ```sh
   scp scripts/install-debian.sh user@vm:/tmp/
   ssh user@vm 'sudo bash /tmp/install-debian.sh'
   ```

   The script installs `curl`, `jq`, `ca-certificates` (the .deb's runtime
   dependencies), then fetches the latest release's `.deb` for the host
   architecture and installs it via `apt`.

### Upgrades

After the initial install, `pgroll-update` is on the system `$PATH`:

```sh
sudo pgroll-update                            # latest release
sudo pgroll-update --version v0.16.1-baselayer.4   # pinned version (also rollback)
```

