#!/usr/bin/env bash
# Extrae .debug de un binario y lo deposita en el layout que debuginfod escanea.
# Uso: ./upload.sh <path-al-binario> [--with-executable]

set -euo pipefail

BINARY="${1:-}"
WITH_EXEC="${2:-}"
STORE="${STORE:-./debuginfo-store}"

if [[ -z "$BINARY" || ! -f "$BINARY" ]]; then
    echo "uso: $0 <path-al-binario> [--with-executable]"
    echo "  STORE=<dir>  override del directorio destino (default: ./debuginfo-store)"
    exit 1
fi

# 1. Extraer build-id
BUILD_ID=$(readelf -n "$BINARY" 2>/dev/null | awk '/Build ID/ { print $3 }')
if [[ -z "$BUILD_ID" ]]; then
    echo "ERROR: '$BINARY' no tiene build-id."
    echo "       Asegurate de compilar con -g y SIN -Wl,--build-id=none"
    exit 1
fi

echo "Binario:  $BINARY"
echo "Build ID: $BUILD_ID"

# 2. Layout que debuginfod escanea (manpage debuginfod(8)):
#    <root>/.build-id/<2-primeras-hex>/<resto-hex>.debug   ← debuginfo
#    <root>/.build-id/<2-primeras-hex>/<resto-hex>         ← executable (opcional)
PREFIX="${BUILD_ID:0:2}"
REST="${BUILD_ID:2}"
TARGET_DIR="$STORE/.build-id/$PREFIX"
DEBUG_FILE="$TARGET_DIR/${REST}.debug"
EXEC_FILE="$TARGET_DIR/${REST}"

mkdir -p "$TARGET_DIR"

# 3. Extraer debug info
objcopy --only-keep-debug "$BINARY" "$DEBUG_FILE"
echo "  → $DEBUG_FILE  ($(wc -c < "$DEBUG_FILE") bytes)"

# 4. Opcional: el binario completo, si querés probar /executable también
if [[ "$WITH_EXEC" == "--with-executable" ]]; then
    cp "$BINARY" "$EXEC_FILE"
    echo "  → $EXEC_FILE  ($(wc -c < "$EXEC_FILE") bytes)"
fi

echo
echo "Probalo (esperá ~10s a que el server lo indexe):"
echo "  curl -fsSL http://localhost:8002/buildid/$BUILD_ID/debuginfo -o /tmp/check.debug"
echo "  readelf -S /tmp/check.debug | grep debug"
