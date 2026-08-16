#!/usr/bin/env bash
# Prueba el flujo end-to-end:
#  1. health del server
#  2. lookup del build-id que subimos
#  3. lookup federado (build-id de glibc Debian) para verificar el upstream
#
# Uso: ./test.sh <path-al-binario>

set -euo pipefail

BINARY="${1:-sample/hello}"
SERVER="${SERVER:-http://localhost:8002}"

if [[ ! -f "$BINARY" ]]; then
    echo "ERROR: no encuentro '$BINARY'. Compilá primero: make -C sample"
    exit 1
fi

BUILD_ID=$(readelf -n "$BINARY" | awk '/Build ID/ { print $3 }')

echo "================================================================"
echo "  1. Server health"
echo "================================================================"
if curl -fsS "$SERVER/metrics" | head -10; then
    echo "  ✓ server responde"
else
    echo "  ✗ server no responde — está corriendo?  docker compose ps"
    exit 1
fi
echo

echo "================================================================"
echo "  2. Lookup local: build-id = $BUILD_ID"
echo "================================================================"
TMP=$(mktemp)
trap 'rm -f "$TMP"' EXIT

if curl -fsSL "$SERVER/buildid/$BUILD_ID/debuginfo" -o "$TMP"; then
    SIZE=$(wc -c < "$TMP")
    echo "  ✓ recibido: $SIZE bytes"
    echo
    echo "  Secciones DWARF en el blob recibido:"
    readelf -S "$TMP" 2>/dev/null \
      | awk '/\.debug|\.symtab|\.strtab/ { printf "    %s\n", $0 }' \
      | head -10 \
      || echo "    (no se pudieron leer secciones)"
else
    echo "  ✗ no encontró el build-id"
    echo "    - corriste ./upload.sh ?"
    echo "    - esperaste el rescan (--rescan-time=10s en el compose)?"
    exit 1
fi
echo

echo "================================================================"
echo "  3. Federación: pedimos un build-id que solo upstream tiene"
echo "================================================================"
echo "  Sacando build-id de libc6 desde un container Debian trixie..."
GLIBC_BID=$(docker run --rm debian:trixie-slim bash -c '
    apt-get install -qy --no-install-recommends libelf1 binutils >/dev/null 2>&1
    readelf -n /lib/x86_64-linux-gnu/libc.so.6 2>/dev/null | awk "/Build ID/{print \$3}"
' 2>/dev/null || echo "")

if [[ -z "$GLIBC_BID" ]]; then
    echo "  ⚠  no pude sacar el build-id de libc (¿docker disponible?)"
    echo "     Te salteo este paso."
else
    echo "  Build ID libc: $GLIBC_BID"
    if curl -fsSL --max-time 30 "$SERVER/buildid/$GLIBC_BID/debuginfo" -o /tmp/libc.debug 2>&1; then
        echo "  ✓ federación OK — debuginfod.debian.net respondió"
        echo "    Tamaño: $(wc -c < /tmp/libc.debug) bytes"
        rm -f /tmp/libc.debug
    else
        echo "  ⚠  upstream no tenía ese build-id exacto (puede pasar)"
    fi
fi
echo

echo "================================================================"
echo "  Listo. Para integrar con un cliente:"
echo "================================================================"
cat <<EOF
  export DEBUGINFOD_URLS="$SERVER"
  gdb $BINARY              # gdb va a buscar símbolos automáticamente
  perf script              # idem si tenés perf data
  eu-stack -p \$(pidof hello)  # idem
EOF
