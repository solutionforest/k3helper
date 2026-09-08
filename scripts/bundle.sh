#!/usr/bin/env bash
# Build a paste-able offline bundle of k3helper for a server that has no
# outbound internet and is only reachable through a provider's web console.
#
# Produces dist/bundle/: a numbered set of shell snippets. Paste each one into
# the console in order; the last one decodes, verifies and installs the binary.
#
#   scripts/bundle.sh                       # linux/amd64, gzip
#   PLATFORM=linux/arm64 scripts/bundle.sh  # other arch
#   COMPRESS=xz scripts/bundle.sh           # ~20% smaller, needs xz on target
#   CHUNK_LINES=800 scripts/bundle.sh       # smaller pastes for strict consoles
set -euo pipefail

PLATFORM="${PLATFORM:-linux/amd64}"
COMPRESS="${COMPRESS:-gzip}"
CHUNK_LINES="${CHUNK_LINES:-2000}"   # 2000 x 76 chars ~= 152 KB per paste
VERSION="${VERSION:-dev}"
OUT_DIR="${OUT_DIR:-dist/bundle}"

GOOS="${PLATFORM%/*}"
GOARCH="${PLATFORM#*/}"

case "$COMPRESS" in
  gzip) COMP_CMD=(gzip -9 -c);  DECOMP="gzip -dc";  EXT="gz" ;;
  xz)   COMP_CMD=(xz -9 -c);    DECOMP="xz -dc";    EXT="xz" ;;
  *) echo "error: COMPRESS must be gzip or xz, got $COMPRESS" >&2; exit 1 ;;
esac

command -v "$COMPRESS" >/dev/null || { echo "error: $COMPRESS not installed" >&2; exit 1; }

sha256() {
  if command -v sha256sum >/dev/null; then sha256sum "$1" | awk '{print $1}'
  else shasum -a 256 "$1" | awk '{print $1}'; fi
}

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

echo "building k3helper $VERSION for $GOOS/$GOARCH (stripped)..."
# -s -w drops the symbol table and DWARF: ~25% smaller, and Go panics still
# carry function names because the runtime keeps its own tables.
GOOS="$GOOS" GOARCH="$GOARCH" go build \
  -ldflags "-s -w -X github.com/solutionforest/k3helper/internal/cli.version=$VERSION" \
  -o "$WORK/k3helper" ./cmd/k3helper

BIN_SUM=$(sha256 "$WORK/k3helper")
BIN_SIZE=$(wc -c < "$WORK/k3helper" | tr -d ' ')

echo "compressing with $COMPRESS and base64-encoding..."
"${COMP_CMD[@]}" "$WORK/k3helper" > "$WORK/k3helper.$EXT"
# tr+fold instead of `base64 -w`: BSD base64 (macOS) has no -w flag.
# The trailing awk guarantees a final newline — fold omits it, but the here-doc
# in each chunk adds one, which would make the line count check off by one.
base64 < "$WORK/k3helper.$EXT" | tr -d '\n' | fold -w 76 | awk '{print}' > "$WORK/payload.b64"

TOTAL_LINES=$(wc -l < "$WORK/payload.b64" | tr -d ' ')
CHUNKS=$(( (TOTAL_LINES + CHUNK_LINES - 1) / CHUNK_LINES ))

rm -rf "$OUT_DIR"
mkdir -p "$OUT_DIR"

# Split into chunk files without relying on `split -d` (not on older BSD split).
awk -v n="$CHUNK_LINES" -v dir="$WORK" '
  { if ((NR-1) % n == 0) { part++; file = sprintf("%s/part-%03d", dir, part) }
    print > file }
' "$WORK/payload.b64"

STAGE="/tmp/k3helper-bundle"
for i in $(seq 1 "$CHUNKS"); do
  part=$(printf 'part-%03d' "$i")
  out=$(printf '%s/%03d-paste.sh' "$OUT_DIR" "$i")
  {
    printf '# k3helper %s %s/%s — paste %d of %d\n' "$VERSION" "$GOOS" "$GOARCH" "$i" "$CHUNKS"
    if [ "$i" -eq 1 ]; then
      printf 'mkdir -p %s && : > %s/payload.b64\n' "$STAGE" "$STAGE"
    fi
    printf "cat >> %s/payload.b64 <<'K3H_EOF'\n" "$STAGE"
    cat "$WORK/$part"
    printf 'K3H_EOF\n'
    # shellcheck disable=SC2016  # $(wc -l) must stay literal in the emitted snippet
    printf 'echo "chunk %d/%d ok — $(wc -l < %s/payload.b64) of %d lines"\n' \
      "$i" "$CHUNKS" "$STAGE" "$TOTAL_LINES"
  } > "$out"
done

# Final snippet: decode, decompress, verify, install.
cat > "$OUT_DIR/999-install.sh" <<EOF
# k3helper $VERSION $GOOS/$GOARCH — final step: decode, verify, install.
set -eu
STAGE=$STAGE
EXPECTED_LINES=$TOTAL_LINES
EXPECTED_SUM=$BIN_SUM
EXPECTED_SIZE=$BIN_SIZE

ACTUAL_LINES=\$(wc -l < "\$STAGE/payload.b64" | tr -d ' ')
if [ "\$ACTUAL_LINES" -ne "\$EXPECTED_LINES" ]; then
  echo "error: payload has \$ACTUAL_LINES lines, expected \$EXPECTED_LINES — a paste was dropped or truncated" >&2
  exit 1
fi

# GNU base64 uses -d, BSD base64 uses -D.
if base64 -d < /dev/null >/dev/null 2>&1; then B64D="base64 -d"; else B64D="base64 -D"; fi
\$B64D < "\$STAGE/payload.b64" > "\$STAGE/k3helper.$EXT"
$DECOMP "\$STAGE/k3helper.$EXT" > "\$STAGE/k3helper"

ACTUAL_SIZE=\$(wc -c < "\$STAGE/k3helper" | tr -d ' ')
[ "\$ACTUAL_SIZE" -eq "\$EXPECTED_SIZE" ] || { echo "error: size \$ACTUAL_SIZE != \$EXPECTED_SIZE" >&2; exit 1; }

if command -v sha256sum >/dev/null; then ACTUAL_SUM=\$(sha256sum "\$STAGE/k3helper" | awk '{print \$1}')
elif command -v shasum >/dev/null; then ACTUAL_SUM=\$(shasum -a 256 "\$STAGE/k3helper" | awk '{print \$1}')
else ACTUAL_SUM=""; echo "warning: no sha256 tool, skipping checksum" >&2; fi
if [ -n "\$ACTUAL_SUM" ] && [ "\$ACTUAL_SUM" != "\$EXPECTED_SUM" ]; then
  echo "error: checksum mismatch" >&2; exit 1
fi

BIN_DIR=\${K3HELPER_BIN_DIR:-/usr/local/bin}
SUDO=""
if [ "\$(id -u)" -ne 0 ]; then
  # If BIN_DIR does not exist yet, writability of its parent is what matters.
  PROBE="\$BIN_DIR"
  [ -d "\$PROBE" ] || PROBE=\$(dirname "\$BIN_DIR")
  [ -w "\$PROBE" ] || SUDO=sudo
fi
\$SUDO mkdir -p "\$BIN_DIR"
\$SUDO install -m 0755 "\$STAGE/k3helper" "\$BIN_DIR/k3helper"
rm -rf "\$STAGE"
echo "installed \$BIN_DIR/k3helper"
"\$BIN_DIR/k3helper" version
EOF

cat > "$OUT_DIR/000-README.txt" <<EOF
k3helper $VERSION — offline paste bundle for $GOOS/$GOARCH
compression: $COMPRESS   chunks: $CHUNKS   payload: $TOTAL_LINES base64 lines
binary sha256: $BIN_SUM

For a server with no outbound internet, reachable only through a web console.
If the server CAN reach the internet, do not use this — run install.sh instead.

1. Open a root shell in the web console.
2. Paste the contents of each NNN-paste.sh file, in numeric order.
   Each one prints "chunk N/$CHUNKS ok" with a running line count. If a count
   does not advance by the expected amount, re-paste that chunk before moving on.
3. Paste 999-install.sh last. It refuses to install unless the line count,
   byte size and sha256 all match, so a dropped paste fails loudly.

Consoles that reject large pastes: rebuild with a smaller chunk size, e.g.
  CHUNK_LINES=500 scripts/bundle.sh
Targets without gzip: COMPRESS=xz (smaller, but needs xz on the target).
EOF

echo
echo "wrote $OUT_DIR/ — $CHUNKS paste chunks + installer"
echo "  binary:  $BIN_SIZE bytes, sha256 $BIN_SUM"
echo "  payload: $TOTAL_LINES base64 lines ($CHUNK_LINES per chunk)"
echo "  start at $OUT_DIR/000-README.txt"
