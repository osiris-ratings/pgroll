#!/usr/bin/env bash
# pgroll-update — Install or upgrade pgroll on a Debian/Ubuntu host.
#
# Reads a GitHub PAT from $PGROLL_RELEASE_TOKEN or /etc/pgroll/release-token,
# fetches the matching .deb from the osiris-ratings/pgroll private GitHub
# release, and installs it via apt.

set -euo pipefail

REPO="osiris-ratings/pgroll"
TOKEN_FILE="/etc/pgroll/release-token"
VERSION=""

usage() {
  cat >&2 <<'EOF'
Usage: pgroll-update [--version <tag>] [-h|--help]

Installs or upgrades pgroll from the private GitHub release.

Options:
  --version <tag>   Install a specific tag (e.g. v0.16.1-baselayer.4).
                    Defaults to the latest release.
  -h, --help        Show this help.

Token resolution (first match wins):
  1. $PGROLL_RELEASE_TOKEN environment variable
  2. /etc/pgroll/release-token (file, mode 0600)
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

for cmd in curl jq dpkg; do
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "error: required command not found: $cmd" >&2
    exit 1
  fi
done

if [[ -n "${PGROLL_RELEASE_TOKEN:-}" ]]; then
  TOKEN="$PGROLL_RELEASE_TOKEN"
elif [[ -r "$TOKEN_FILE" ]]; then
  TOKEN="$(cat "$TOKEN_FILE")"
else
  echo "error: no token. Set \$PGROLL_RELEASE_TOKEN or populate $TOKEN_FILE (mode 0600)." >&2
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

if [[ "$(id -u)" -ne 0 ]]; then
  SUDO=sudo
else
  SUDO=
fi

echo "Installing ${ASSET_NAME}..."
$SUDO apt-get install -y "$DEB"

echo "Installed: $(pgroll --version 2>/dev/null || echo '(pgroll --version failed)')"
