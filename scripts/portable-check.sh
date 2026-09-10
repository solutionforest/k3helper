#!/bin/sh
# Assert that a built k3helper is actually portable: it runs from a directory
# it has never seen, with nothing installed, and writes nothing outside that
# directory.
#
#   scripts/portable-check.sh ./bin/k3helper
#
# "Single portable binary" is the first line of the README. It is a claim about
# the runtime, not the build, so it has to be checked by running the thing —
# a cross-compile that succeeds says nothing about whether the binary needs a
# config directory, a writable home, or a shell that is not there.
#
# POSIX sh: this runs on Linux and macOS runners, and by hand on a developer's
# machine. The Windows equivalent lives in the workflow, in PowerShell, so that
# nothing POSIX is involved in proving the .exe stands alone.
set -eu

BIN="${1:-}"
[ -n "$BIN" ] || { echo "usage: $0 <path-to-k3helper>" >&2; exit 1; }

# Resolve before leaving the current directory.
case "$BIN" in
  /*) ;;
  *) BIN="$(pwd)/$BIN" ;;
esac
[ -x "$BIN" ] || { echo "error: $BIN is not executable" >&2; exit 1; }

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT INT TERM
cd "$WORK"

pass() { printf '  ✓ %s\n' "$*"; }
fail() { printf '  ✗ %s\n' "$*" >&2; exit 1; }

echo "portable check: $BIN"
echo "  in: $WORK"

# --- it runs at all -------------------------------------------------------
"$BIN" version >/dev/null || fail "version failed"
pass "version"

"$BIN" --help >/dev/null || fail "--help failed"
pass "--help"

# --- a missing targets file is explained, not stack-traced ----------------
if "$BIN" ctx >/dev/null 2>&1; then
  fail "ctx succeeded in an empty directory with no targets file"
fi
out=$("$BIN" ctx 2>&1 || true)
case "$out" in
  *"no targets file"*) pass "missing targets file explains itself" ;;
  *) fail "unhelpful error for a missing targets file: $out" ;;
esac

# --- a kubeconfig that is not there is refused, and nothing is written ----
# Paths are relative on purpose: we are already in $WORK, and Git Bash on
# Windows rewrites POSIX-looking absolute arguments on their way to a native
# binary. A relative path takes that translation out of the test, which is
# about k3helper rather than about MSYS.
if "$BIN" init --kubeconfig missing.yaml --cluster demo >/dev/null 2>&1; then
  fail "init accepted a kubeconfig that does not exist"
fi
[ -f targets.yaml ] && fail "init wrote a targets file after failing"
pass "missing kubeconfig refused, nothing written"

# --- the kubeconfig flow works end to end --------------------------------
printf 'apiVersion: v1\nkind: Config\n' > kube.yaml
"$BIN" init --kubeconfig kube.yaml --kube-context admin --cluster demo >/dev/null \
  || fail "init failed on a valid kubeconfig"
[ -f targets.yaml ] || fail "init reported success but wrote no file"
pass "init --kubeconfig"

"$BIN" ctx | grep -q kubeconfig || fail "ctx does not report the cluster as kubeconfig-reached"
pass "ctx reports how the cluster is reached"

# --- SSH mode still works from a bare directory too ----------------------
"$BIN" init --out ssh-targets.yaml --server 10.0.0.10 --agent 10.0.0.11 \
  --user ubuntu --cluster lab >/dev/null || fail "init failed for an SSH cluster"
"$BIN" ctx -t ssh-targets.yaml | grep -q ssh || fail "ctx does not report an SSH cluster"
pass "init/ctx for an SSH cluster"

# --- offline YAML work needs no cluster at all ---------------------------
"$BIN" gen deployment web --image nginx:1.27 > app.yaml || fail "gen failed"
"$BIN" verify app.yaml >/dev/null || fail "verify rejected its own generated manifest"
pass "gen + verify with no cluster and no network"

# --- nothing was written outside the working directory -------------------
# The claim is "nothing installed". A config or state directory appearing in
# $HOME is the usual way that stops being true.
for d in "${HOME:-/nonexistent}/.k3helper" "${HOME:-/nonexistent}/.config/k3helper"; do
  [ -e "$d" ] && fail "wrote outside the working directory: $d"
done
pass "nothing written outside the working directory"

echo "portable check passed"
