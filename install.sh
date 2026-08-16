#!/bin/sh
# FORGE installer for macOS and Linux.
#
#   curl -fsSL https://raw.githubusercontent.com/VEER-TARGARYEN/forge/main/install.sh | sh
#
# It downloads the installer for your platform from the latest GitHub release,
# checks it against the published SHA256SUMS, and runs it. Everything lands
# under your home directory: no sudo, and nothing outside your own profile.
#
# Piping a script into a shell is a thing to be suspicious of. This one is
# short on purpose so it can be read first:
#
#   curl -fsSL https://raw.githubusercontent.com/VEER-TARGARYEN/forge/main/install.sh -o install.sh
#   less install.sh && sh install.sh
#
# Environment:
#   FORGE_VERSION   install a specific tag (default: latest)
#   FORGE_DIR       install to a specific directory
#   FORGE_NO_VERIFY set to 1 to skip checksum verification (not recommended)

set -eu

REPO="VEER-TARGARYEN/forge"
APP="FORGE"

say()  { printf '%s\n' "$*"; }
warn() { printf '%s\n' "$*" >&2; }
die()  { printf '\nerror: %s\n' "$*" >&2; exit 1; }

need() {
    command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed"
}

# ---- fetch helper: curl or wget, whichever exists ----------------------------

if command -v curl >/dev/null 2>&1; then
    fetch()      { curl -fsSL "$1"; }
    fetch_file() { curl -fsSL "$1" -o "$2"; }
elif command -v wget >/dev/null 2>&1; then
    fetch()      { wget -qO- "$1"; }
    fetch_file() { wget -qO "$2" "$1"; }
else
    die "either curl or wget is required"
fi

# ---- platform ---------------------------------------------------------------

os=$(uname -s)
case "$os" in
    Darwin) OS=darwin ;;
    Linux)  OS=linux ;;
    *)      die "unsupported operating system: $os
Windows users: see https://github.com/$REPO#install" ;;
esac

arch=$(uname -m)
case "$arch" in
    x86_64|amd64)   ARCH=amd64 ;;
    arm64|aarch64)  ARCH=arm64 ;;
    *)              die "unsupported architecture: $arch" ;;
esac

# ---- resolve the release ----------------------------------------------------

VERSION="${FORGE_VERSION:-}"
if [ -z "$VERSION" ]; then
    say "Finding the latest release..."
    # Parse the tag out of the API response rather than adding a jq dependency
    # to a script whose whole point is not needing anything installed.
    VERSION=$(fetch "https://api.github.com/repos/$REPO/releases/latest" \
        | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
        | head -n 1)
    [ -n "$VERSION" ] || die "could not determine the latest release.
Set FORGE_VERSION=v1.0.0 to pick one explicitly."
fi

ASSET="$APP-setup-$OS-$ARCH"
BASE="https://github.com/$REPO/releases/download/$VERSION"

say "$APP $VERSION  ($OS/$ARCH)"

# ---- download ---------------------------------------------------------------

TMP=$(mktemp -d 2>/dev/null || mktemp -d -t forge)
trap 'rm -rf "$TMP"' EXIT INT TERM

say "Downloading $ASSET..."
fetch_file "$BASE/$ASSET" "$TMP/$ASSET" \
    || die "no build for $OS/$ARCH in $VERSION.
See https://github.com/$REPO/releases/$VERSION for what is available."

# ---- verify -----------------------------------------------------------------

if [ "${FORGE_NO_VERIFY:-0}" = "1" ]; then
    warn "Skipping checksum verification (FORGE_NO_VERIFY=1)."
else
    if fetch_file "$BASE/SHA256SUMS" "$TMP/SHA256SUMS" 2>/dev/null; then
        expected=$(grep " $ASSET\$" "$TMP/SHA256SUMS" | awk '{print $1}')
        if [ -z "$expected" ]; then
            warn "warning: $ASSET is not listed in SHA256SUMS; continuing unverified."
        else
            if command -v sha256sum >/dev/null 2>&1; then
                actual=$(sha256sum "$TMP/$ASSET" | awk '{print $1}')
            elif command -v shasum >/dev/null 2>&1; then
                actual=$(shasum -a 256 "$TMP/$ASSET" | awk '{print $1}')
            else
                actual=""
                warn "warning: no sha256 tool found; continuing unverified."
            fi
            if [ -n "$actual" ]; then
                [ "$actual" = "$expected" ] || die "checksum mismatch for $ASSET.
  expected $expected
  got      $actual
Do not run it. Please open an issue at https://github.com/$REPO/issues"
                say "Checksum verified."
            fi
        fi
    else
        warn "warning: SHA256SUMS not published for $VERSION; continuing unverified."
    fi
fi

# ---- install ----------------------------------------------------------------

chmod +x "$TMP/$ASSET"
say ""
if [ -n "${FORGE_DIR:-}" ]; then
    "$TMP/$ASSET" -dir "$FORGE_DIR" -launch=false
else
    "$TMP/$ASSET" -launch=false
fi

# ---- afterwards -------------------------------------------------------------

say ""
if ! command -v forge >/dev/null 2>&1; then
    case ":$PATH:" in
        *":$HOME/.local/bin:"*) ;;
        *) say "Note: $HOME/.local/bin is not on your PATH. Add it with:"
           say ""
           say "    echo 'export PATH=\"\$HOME/.local/bin:\$PATH\"' >> ~/.profile"
           say "" ;;
    esac
fi
say "Get started:"
say ""
say "    forge init          # write a starter config"
say "    forge doctor        # check which providers are reachable"
say "    forge app           # open the desktop interface"
say ""
