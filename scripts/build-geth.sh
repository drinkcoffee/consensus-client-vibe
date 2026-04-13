#!/usr/bin/env bash
# build-geth.sh — ensures the pinned geth binary exists.
#
# The script is idempotent: it is a no-op if the binary is already present.
# On a fresh checkout it initialises the go-ethereum git submodule and builds
# the geth binary.  The binary is placed at:
#
#   third_party/go-ethereum/build/bin/geth
#
# Usage:
#   ./scripts/build-geth.sh          # build if not already present
#   ./scripts/build-geth.sh --force  # always rebuild
#
# Requirements: git, go (same major version as the project go.mod)

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SUBMODULE_DIR="$REPO_ROOT/third_party/go-ethereum"
GETH_BIN="$SUBMODULE_DIR/build/bin/geth"

FORCE=false
if [[ "${1:-}" == "--force" ]]; then
    FORCE=true
fi

# ── Already built? ────────────────────────────────────────────────────────────

if [[ -f "$GETH_BIN" && "$FORCE" == "false" ]]; then
    echo "geth already built: $GETH_BIN"
    "$GETH_BIN" version 2>&1 | head -1
    exit 0
fi

# ── Initialise submodule if the source tree is missing ────────────────────────

if [[ ! -f "$SUBMODULE_DIR/go.mod" ]]; then
    echo "Initialising go-ethereum submodule (shallow clone) …"
    git -C "$REPO_ROOT" submodule update --init --depth=1 third_party/go-ethereum
fi

# ── Build ─────────────────────────────────────────────────────────────────────

echo "Building geth from $(git -C "$SUBMODULE_DIR" describe --tags 2>/dev/null || echo unknown) …"
mkdir -p "$SUBMODULE_DIR/build/bin"
(cd "$SUBMODULE_DIR" && go build -o build/bin/geth ./cmd/geth)

echo ""
echo "Built: $GETH_BIN"
"$GETH_BIN" version 2>&1 | head -1
