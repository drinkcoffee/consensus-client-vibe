#!/usr/bin/env bash
# setup.sh — build images, generate keys, initialise Geth data directories,
# and start the four-node Clique PoA demo.
#
# Usage:
#   ./setup.sh           # first-time setup and start
#   ./setup.sh --reset   # tear down, wipe generated data, and start fresh
#   ./setup.sh --stop    # stop all containers (keeps data)
#   ./setup.sh --logs    # tail logs from all containers
#
# Requirements: docker (with Compose v2 plugin), go (for local build check only)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
GENERATED="${SCRIPT_DIR}/generated"
COMPOSE="docker compose -p clique-demo -f ${SCRIPT_DIR}/docker-compose.yml"

# ── Argument handling ────────────────────────────────────────────────────────

case "${1:-}" in
  --stop)
    echo "Stopping demo..."
    $COMPOSE down
    exit 0
    ;;
  --logs)
    $COMPOSE logs -f
    exit 0
    ;;
  --reset)
    echo "Resetting demo (removing containers and generated data)..."
    $COMPOSE down -v 2>/dev/null || true
    rm -rf "${GENERATED}" "${SCRIPT_DIR}/data"
    echo "Reset complete. Re-run ./setup.sh to start fresh."
    exit 0
    ;;
  "")
    ;;
  *)
    echo "Unknown argument: ${1}" >&2
    echo "Usage: $0 [--reset | --stop | --logs]" >&2
    exit 1
    ;;
esac

# ── Dependency checks ────────────────────────────────────────────────────────

check_cmd() {
  if ! command -v "$1" &>/dev/null; then
    echo "Error: '$1' is required but not found." >&2
    exit 1
  fi
}

check_cmd docker

if ! docker compose version &>/dev/null; then
  echo "Error: Docker Compose v2 plugin not found. Install Docker Desktop or the compose plugin." >&2
  exit 1
fi

# ── Step 1: Build images ─────────────────────────────────────────────────────

echo ""
echo "==> Building clique-node image..."
docker build -t clique-node:demo \
  -f "${PROJECT_ROOT}/Dockerfile" \
  "${PROJECT_ROOT}"

echo ""
echo "==> Building keygen image..."
docker build -t clique-node-keygen \
  -f "${SCRIPT_DIR}/init/Dockerfile" \
  "${PROJECT_ROOT}"

# ── Step 2: Generate keys and configuration ──────────────────────────────────

if [[ -d "${GENERATED}" ]]; then
  echo ""
  echo "==> Generated directory exists — skipping key generation."
  echo "    (Run ./setup.sh --reset to regenerate everything from scratch.)"
else
  echo ""
  echo "==> Generating keys, genesis block, and node configuration..."
  docker run --rm \
    -v "${GENERATED}:/generated" \
    clique-node-keygen \
    /generated
fi

# ── Step 3: Initialise Geth data directories ────────────────────────────────

for NODE in 1 2 3 4; do
  DATA_DIR="${SCRIPT_DIR}/data/geth-${NODE}"
  CHAINDATA="${DATA_DIR}/geth/chaindata"

  if [[ -d "${CHAINDATA}" ]]; then
    echo "==> geth-${NODE}: already initialised, skipping."
    continue
  fi

  echo ""
  echo "==> Initialising geth-${NODE} with genesis block..."
  mkdir -p "${DATA_DIR}"
  docker run --rm \
    -v "${GENERATED}:/generated:ro" \
    -v "${DATA_DIR}:/data" \
    ethereum/client-go:latest \
    init --datadir /data /generated/genesis.json

  # Place the pre-generated devp2p nodekey and static-peers list where Geth
  # expects them: <datadir>/geth/nodekey and <datadir>/geth/static-nodes.json
  mkdir -p "${DATA_DIR}/geth"
  cp "${GENERATED}/geth-${NODE}/nodekey"           "${DATA_DIR}/geth/nodekey"
  cp "${GENERATED}/geth-${NODE}/static-nodes.json" "${DATA_DIR}/geth/static-nodes.json"
  # Restrict nodekey permissions
  chmod 600 "${DATA_DIR}/geth/nodekey"
done

# ── Step 4: Start all services ───────────────────────────────────────────────

echo ""
echo "==> Starting all services..."
$COMPOSE up -d

# ── Step 5: Print access info ────────────────────────────────────────────────

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  Clique PoA demo is starting up."
echo ""
echo "  Execution client JSON-RPC endpoints:"
echo "    geth-1 (primary):  http://localhost:8545"
echo "    geth-2:            http://localhost:8546"
echo "    geth-3:            http://localhost:8547"
echo "    geth-4 (observer): http://localhost:8548"
echo ""
echo "  Consensus client RPC endpoints:"
echo "    clique-1: http://localhost:5052"
echo "    clique-2: http://localhost:5053"
echo "    clique-3: http://localhost:5054"
echo "    clique-4: http://localhost:5055  (observer)"
echo ""
echo "  Signer addresses and other details: ${GENERATED}/info.txt"
echo ""
echo "  Follow logs:   ./setup.sh --logs"
echo "  Stop:          ./setup.sh --stop"
echo "  Full reset:    ./setup.sh --reset"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
