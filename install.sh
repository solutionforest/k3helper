#!/bin/sh
# k3helper installer.
#
#   curl -fsSL https://raw.githubusercontent.com/solutionforest/k3helper/main/install.sh | sh
#
# Environment overrides:
#   K3HELPER_VERSION   tag to install (default: latest release)
#   K3HELPER_BIN_DIR   install directory (default: /usr/local/bin)
#   K3HELPER_OS        override detected OS   (linux, darwin)
#   K3HELPER_ARCH      override detected arch (amd64, arm64)
#
# POSIX sh on purpose: this is pasted into provider web consoles on minimal
# images where bash is not guaranteed.
set -eu

REPO="solutionforest/k3helper"
BIN_DIR="${K3HELPER_BIN_DIR:-/usr/local/bin}"
BIN_NAME="k3helper"

log()  { printf '%s\n' "$*" >&2; }
fail() { printf 'error: %s\n' "$*" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1; }

# --- fetch helper: curl or wget, whichever exists -------------------------
if need curl; then
  fetch()      { curl -fsSL "$1"; }
  fetch_file() { curl -fsSL "$1" -o "$2"; }
elif need wget; then
  fetch()      { wget -qO- "$1"; }
  fetch_file() { wget -qO "$2" "$1"; }
else
  fail "need curl or wget"
fi

# --- privilege escalation only if the target dir is not writable ----------
SUDO=""
if [ "$(id -u)" -ne 0 ]; then
  # If BIN_DIR does not exist yet, writability of its parent is what matters.
  PROBE="$BIN_DIR"
  [ -d "$PROBE" ] || PROBE=$(dirname "$BIN_DIR")
  if [ ! -w "$PROBE" ]; then
    need sudo || fail "$BIN_DIR is not writable and sudo is not available; set K3HELPER_BIN_DIR to a writable path"
    SUDO="sudo"
  fi
fi

# --- platform detection ---------------------------------------------------
OS="${K3HELPER_OS:-$(uname -s | tr '[:upper:]' '[:lower:]')}"
case "$OS" in
  linux|darwin) ;;
  *) fail "unsupported OS: $OS" ;;
esac

ARCH="${K3HELPER_ARCH:-$(uname -m)}"
case "$ARCH" in
  x86_64|amd64)   ARCH=amd64 ;;
  aarch64|arm64)  ARCH=arm64 ;;
  *) fail "unsupported architecture: $ARCH" ;;
esac

# --- resolve version ------------------------------------------------------
VERSION="${K3HELPER_VERSION:-}"
if [ -z "$VERSION" ]; then
  log "resolving latest release..."
  VERSION=$(fetch "https://api.github.com/repos/$REPO/releases/latest" \
    | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
    | head -n1)
  [ -n "$VERSION" ] || fail "could not resolve latest release; set K3HELPER_VERSION=vX.Y.Z"
fi

ASSET="${BIN_NAME}-${OS}-${ARCH}"
BASE="https://github.com/$REPO/releases/download/$VERSION"

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT INT TERM

log "downloading $ASSET $VERSION..."
fetch_file "$BASE/$ASSET" "$TMP/$BIN_NAME" \
  || fail "download failed: $BASE/$ASSET (does $VERSION publish $OS/$ARCH?)"

# --- checksum verification (skipped only if the release has no manifest) ---
if fetch_file "$BASE/checksums.txt" "$TMP/checksums.txt" 2>/dev/null; then
  EXPECTED=$(grep " $ASSET\$" "$TMP/checksums.txt" | awk '{print $1}' | head -n1)
  if [ -n "$EXPECTED" ]; then
    if need sha256sum;   then ACTUAL=$(sha256sum "$TMP/$BIN_NAME" | awk '{print $1}')
    elif need shasum;    then ACTUAL=$(shasum -a 256 "$TMP/$BIN_NAME" | awk '{print $1}')
    else ACTUAL=""; log "warning: no sha256sum/shasum, skipping checksum verification"
    fi
    if [ -n "$ACTUAL" ]; then
      [ "$ACTUAL" = "$EXPECTED" ] || fail "checksum mismatch for $ASSET (expected $EXPECTED, got $ACTUAL)"
      log "checksum ok"
    fi
  else
    log "warning: $ASSET not listed in checksums.txt, skipping verification"
  fi
else
  log "warning: no checksums.txt in $VERSION, skipping verification"
fi

chmod +x "$TMP/$BIN_NAME"
$SUDO mkdir -p "$BIN_DIR"
$SUDO cp "$TMP/$BIN_NAME" "$BIN_DIR/$BIN_NAME"

log ""
log "installed $BIN_DIR/$BIN_NAME"
"$BIN_DIR/$BIN_NAME" version 2>/dev/null || true
case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *) log "note: $BIN_DIR is not in PATH" ;;
esac
