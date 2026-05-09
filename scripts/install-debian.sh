#!/usr/bin/env bash
# install-debian.sh — One-time bootstrap for pgroll on a Debian/Ubuntu host.
#
# Use this on a fresh VM to install pgroll for the first time. After this,
# /usr/bin/pgroll-update is available for subsequent upgrades — they share
# the same logic, so prefer pgroll-update once the package is on disk.
#
# Token resolution:
#   1. $PGROLL_RELEASE_TOKEN environment variable
#   2. /etc/pgroll/release-token (file, mode 0600)

set -euo pipefail

REPO="osiris-ratings/pgroll"
TOKEN_FILE="/etc/pgroll/release-token"
VERSION=""

usage() {
  cat >&2 <<'EOF'
Usage: install-debian.sh [--version <tag>] [-h|--help]

Installs prerequisites (curl, jq, ca-certificates) and pgroll from the
private GitHub release.

Options:
  --version <tag>   Install a specific tag (e.g. v0.16.1-baselayer.4).
                    Defaults to the latest release.
  -h, --help        Show this help.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --version)
      [[ $# -ge 2 ]] || { echo "error: --version requires a tag" >&2; exit 2; }
      VERSION="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "error: unknown argument: $1" >&2
      usage
      exit 2
      ;;
  esac
done

if [[ "$(id -u)" -ne 0 ]]; then
  SUDO=sudo
else
  SUDO=
fi

if ! command -v dpkg >/dev/null 2>&1; then
  echo "error: dpkg not found — this script requires a Debian-based system." >&2
  exit 1
fi

echo "Ensuring prerequisites (curl, jq, ca-certificates)..."
$SUDO apt-get update
$SUDO apt-get install -y curl jq ca-certificates

if [[ -n "${PGROLL_RELEASE_TOKEN:-}" ]]; then
  TOKEN="$PGROLL_RELEASE_TOKEN"
elif [[ -r "$TOKEN_FILE" ]]; then
  TOKEN="$(cat "$TOKEN_FILE")"
else
  cat >&2 <<EOF
error: no GitHub token available.

Provide one of:
  - export PGROLL_RELEASE_TOKEN=ghp_...   (env var, this shell only)
  - sudo install -d -m 0700 /etc/pgroll
    echo 'ghp_...' | sudo tee $TOKEN_FILE >/dev/null
    sudo chmod 0600 $TOKEN_FILE            (persistent)

Use a fine-grained PAT scoped to ${REPO} with Contents: Read.
EOF
  exit 1
fi

ARCH="$(dpkg --print-architecture)"
case "$ARCH" in
  amd64|arm64) ;;
  *) echo "error: unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

API="https://api.github.com/repos/${REPO}/releases"
if [[ -n "$VERSION" ]]; then
  REL_URL="${API}/tags/${VERSION}"
else
  REL_URL="${API}/latest"
fi

echo "Fetching release metadata from ${REL_URL#${API}/}..."
META="$(curl -fsSL \
  -H "Accept: application/vnd.github+json" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "X-GitHub-Api-Version: 2022-11-28" \
  "$REL_URL")"

TAG="$(printf '%s' "$META" | jq -r '.tag_name')"
ASSET_ID="$(printf '%s' "$META" | jq -r --arg arch "$ARCH" '
  .assets[] | select(.name | test("^pgroll_.*_" + $arch + "\\.deb$")) | .id
' | head -n1)"
ASSET_NAME="$(printf '%s' "$META" | jq -r --arg arch "$ARCH" '
  .assets[] | select(.name | test("^pgroll_.*_" + $arch + "\\.deb$")) | .name
' | head -n1)"

if [[ -z "$ASSET_ID" || "$ASSET_ID" == "null" ]]; then
  echo "error: no .deb asset found for arch=${ARCH} in release ${TAG}." >&2
  exit 1
fi

TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT
DEB="${TMPDIR}/${ASSET_NAME}"
ASSET_URL="https://api.github.com/repos/${REPO}/releases/assets/${ASSET_ID}"

echo "Downloading ${ASSET_NAME} (${TAG})..."
curl -fsSL \
  -H "Accept: application/octet-stream" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "X-GitHub-Api-Version: 2022-11-28" \
  -o "$DEB" \
  "$ASSET_URL"

echo "Installing ${ASSET_NAME}..."
$SUDO apt-get install -y "$DEB"

echo "Installed: $(pgroll --version 2>/dev/null || echo '(pgroll --version failed)')"
echo "Subsequent upgrades: run 'sudo pgroll-update'."
