#!/bin/sh
# bananastand installer — https://github.com/dbreunig/bananastand
#
#   curl -fsSL https://raw.githubusercontent.com/dbreunig/bananastand/main/install.sh | bash
#
# Variants:
#   ... | bash -s -- --run                        # install, then price your system now
#   ... | BANANASTAND_VERSION=v0.1.0 bash            # pin a release
#   ... | BANANASTAND_INSTALL_DIR=$HOME/.local/bin bash
#
# The script picks the binary for your architecture, downloads it from
# the GitHub release, verifies it against the release's SHA256SUMS, and
# installs it. Read it first if you like:
#   curl -fsSL .../install.sh | less

set -eu

REPO="${BANANASTAND_REPO:-dbreunig/bananastand}"
VERSION="${BANANASTAND_VERSION:-latest}"
RUN_AFTER=0
for arg in "$@"; do
  case "$arg" in
    --run) RUN_AFTER=1 ;;
    *) echo "unknown option: $arg" >&2; exit 2 ;;
  esac
done

say() { printf '%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

[ "$(uname -s)" = "Linux" ] || die "bananastand supports Linux only"
case "$(uname -m)" in
  x86_64 | amd64) ARCH=amd64 ;;
  aarch64 | arm64) ARCH=arm64 ;;
  *) die "no prebuilt binary for $(uname -m); build from source with Go" ;;
esac
ASSET="bananastand-linux-$ARCH"

if [ -n "${BANANASTAND_BASE_URL:-}" ]; then
  BASE="$BANANASTAND_BASE_URL" # test/mirror override
elif [ "$VERSION" = "latest" ]; then
  BASE="https://github.com/$REPO/releases/latest/download"
else
  BASE="https://github.com/$REPO/releases/download/$VERSION"
fi

fetch() {
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$1" -o "$2"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$2" "$1"
  else
    die "need curl or wget"
  fi
}

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT INT TERM

say "downloading $ASSET from $BASE"
fetch "$BASE/$ASSET" "$TMP/$ASSET"
fetch "$BASE/SHA256SUMS" "$TMP/SHA256SUMS"

command -v sha256sum >/dev/null 2>&1 || die "sha256sum not found"
(cd "$TMP" && grep " $ASSET\$" SHA256SUMS | sha256sum -c - >/dev/null 2>&1) ||
  die "checksum verification failed; not installing"
say "checksum verified"
chmod +x "$TMP/$ASSET"

# /usr/local/bin is the default so `sudo bananastand` works out of the box —
# root's PATH includes it, and dmidecode wants root.
DEST="${BANANASTAND_INSTALL_DIR:-/usr/local/bin}"
SUDO=""
if [ "$(id -u)" != 0 ] && [ ! -w "$DEST" ]; then
  if command -v sudo >/dev/null 2>&1; then
    SUDO=sudo
    say "installing to $DEST needs sudo"
  elif [ -z "${BANANASTAND_INSTALL_DIR:-}" ]; then
    DEST="$HOME/.local/bin"
    say "no sudo available; installing to $DEST instead"
  else
    die "cannot write to $DEST"
  fi
fi
$SUDO mkdir -p "$DEST"
$SUDO install -m 755 "$TMP/$ASSET" "$DEST/bananastand"

case ":$PATH:" in
  *":$DEST:"*) ;;
  *) say "note: add $DEST to your PATH to run it as 'bananastand'" ;;
esac

say "installed $("$DEST/bananastand" --version) to $DEST/bananastand"
say ""
say "quick start:"
say "  sudo bananastand     # sudo lets dmidecode report DDR gen/ECC/speed"
say "  bananastand --help"
if [ "$RUN_AFTER" = 1 ]; then
  say ""
  "$DEST/bananastand"
fi
