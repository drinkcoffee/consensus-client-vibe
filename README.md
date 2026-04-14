# consensus-client-vibe

An Ethereum consensus client written in Go that supports both [Clique](https://eips.ethereum.org/EIPS/eip-225) (Proof-of-Authority) and [QBFT](https://consensys.io/docs/goquorum/en/stable/reference/consensus/qbft/) (Byzantine Fault Tolerant) consensus. It communicates with any Ethereum execution client via the [Engine API](https://github.com/ethereum/execution-apis/tree/main/src/engine), propagates blocks over a libp2p P2P network, and exposes a JSON-RPC HTTP API for node monitoring and validator management.

This is a vibe-coded project — architecture-first, iteratively built.

## What It Is

Most Ethereum consensus clients (Lighthouse, Prysm, Teku, Nimbus) implement Proof-of-Stake (Gasper). This client instead implements **Clique PoA** or **QBFT**, where a fixed set of authorized validators produce blocks. This makes it suitable for:

- Private/permissioned Ethereum networks
- Local development chains (replacing the consensus engine embedded in Geth)
- Learning how the consensus/execution client split works post-Merge

The split architecture means the execution client (Geth, Nethermind, etc.) handles transaction processing, the EVM, and state, while this client handles block ordering, signing, and fork choice.

## Architecture

```
┌───────────────────────────────────────────────────┐
│                   clique-node                      │
│                                                    │
│  ┌───────────┐   ┌──────────┐   ┌──────────────┐ │
│  │  JSON RPC │   │   P2P    │   │  Engine API  │ │
│  │  Server   │   │ (libp2p) │   │  Client (EL) │ │
│  └─────┬─────┘   └────┬─────┘   └──────┬───────┘ │
│        │              │                │           │
│  ┌─────▼──────────────▼────────────────▼───────┐  │
│  │                 Node Core                    │  │
│  │   ┌──────────────┐   ┌────────────────────┐ │  │
│  │   │    Clique    │   │    Fork Choice     │ │  │
│  │   │   Consensus  │   │  (heaviest chain)  │ │  │
│  │   └──────────────┘   └────────────────────┘ │  │
│  └──────────────────────────────────────────────┘  │
└───────────────────────────────────────────────────┘
         │  Engine API (JWT-authenticated HTTP)
         ▼
┌───────────────────────────────────────────────────┐
│     Execution Client (Geth / Nethermind / etc.)   │
└───────────────────────────────────────────────────┘
```

## Build

Requires Go 1.21+.

```bash
git clone https://github.com/peterrobinson/consensus-client-vibe
cd consensus-client-vibe
go build ./cmd/clique-node
```

## Configuration

Copy the example config and edit it for your network:

```bash
cp config.example.toml config.toml
$EDITOR config.toml
```

### Clique (default)

| Section | Field | Description |
|---|---|---|
| `[node]` | `network_id` | Must match your execution client's chain/network ID |
| `[engine]` | `url` | Engine API endpoint of your execution client (default `:8551`) |
| `[engine]` | `jwt_secret_path` | Path to the shared JWT secret file (hex-encoded) |
| `[engine]` | `el_rpc_url` | Regular JSON-RPC endpoint used to fetch the genesis block on startup (default `http://localhost:8545`) |
| `[consensus.clique]` | `signer_key_path` | ECDSA private key for signing blocks; omit for follower mode |
| `[consensus.clique]` | `period` | Seconds between blocks — must match `genesis.config.clique.period` |
| `[consensus.clique]` | `epoch` | Blocks per epoch — must match `genesis.config.clique.epoch` |
| `[p2p]` | `listen_addr` | libp2p multiaddr to listen on, e.g. `/ip4/0.0.0.0/tcp/9000` |
| `[rpc]` | `listen_addr` | JSON-RPC HTTP server address, e.g. `0.0.0.0:5052` |

### QBFT

QBFT (Istanbul BFT) is a Byzantine Fault Tolerant consensus protocol. Unlike Clique, where any single signer can produce a block unilaterally, QBFT requires a quorum of validators (⌊2N/3⌋ + 1) to explicitly prepare and commit each block before it is final. This makes it suitable for networks that need immediate finality and stronger safety guarantees.

#### 1. Create the genesis

Use a standard Geth Clique-format genesis, placing the sorted validator addresses in `extraData`:

```
extraData = 0x{32 zero bytes}{validator address 1}{validator address 2}...{65 zero bytes}
```

All validator addresses must be sorted lexicographically (ascending by their hex string). The QBFT engine reads the initial validator set from this field.

```json
{
  "config": {
    "chainId": 12345,
    "clique": { "period": 4, "epoch": 30000 },
    "terminalTotalDifficulty": 0,
    "terminalTotalDifficultyPassed": true,
    ...
  },
  "extraData": "0x0000000000000000000000000000000000000000000000000000000000000000<addr1><addr2><addr3>0000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000",
  "difficulty": "0x1",
  ...
}
```

Initialise Geth normally with `geth init genesis.json`.

#### 2. Generate validator keys

Each validator needs an ECDSA private key stored as a hex string (no `0x` prefix):

```bash
# Using openssl (any 32-byte random key works)
openssl rand -hex 32 > validator.hex

# Or extract from a Geth keystore using geth account tools
```

#### 3. Configure each node

Set `[consensus] type = "qbft"` and fill in the `[consensus.qbft]` section:

```toml
[consensus]
type = "qbft"

[consensus.qbft]
# Path to the hex-encoded ECDSA private key for this validator.
# Omit (or leave empty) to run in follower (non-voting) mode.
validator_key_path = "./validator.hex"

# Minimum seconds between blocks — must match genesis.config.clique.period.
period = 4

# Blocks per epoch (validator-set checkpoint interval).
# Must match genesis.config.clique.epoch.
epoch = 30000

# Round timeout in milliseconds. If no block is committed within this window
# the validator broadcasts ROUND_CHANGE and the round advances.
# A value of 4000–10000 ms is typical for LAN/local networks.
request_timeout_ms = 4000
```

| Field | Description |
|---|---|
| `validator_key_path` | ECDSA private key for signing proposals and commits; omit for follower mode |
| `period` | Minimum seconds between blocks |
| `epoch` | Blocks between validator-set checkpoints (embed validators in header extra) |
| `request_timeout_ms` | Round timeout before triggering ROUND_CHANGE |

#### 4. Wire the nodes together

Each node needs to know the P2P addresses of the other nodes. Start each node once to obtain its address (printed at startup), then set `boot_nodes` in `[p2p]`:

```toml
[p2p]
listen_addr = "/ip4/0.0.0.0/tcp/9000"
boot_nodes = [
  "/ip4/1.2.3.4/tcp/9000/p2p/12D3KooW...",
  "/ip4/1.2.3.5/tcp/9000/p2p/12D3KooW...",
]
```

Follower nodes (no `validator_key_path`) receive committed blocks via P2P gossip and track the canonical chain without participating in the protocol.

## Usage

```bash
# Run with a config file
./clique-node --config config.toml

# Override log level at runtime
./clique-node --config config.toml --log-level debug

# JSON log output (for structured log ingestion)
./clique-node --config config.toml --log-format json

# Print all flags
./clique-node --help
```

The node reads its config on startup, falls back to defaults if no config file is found, and shuts down cleanly on `SIGINT` / `SIGTERM`.

## JSON-RPC API

The following endpoints are available. See [docs/RPC.md](docs/RPC.md) for full request/response documentation.

| Endpoint | Description |
|---|---|
| `GET /eth/v1/node/identity` | libp2p peer ID, ENR, and multiaddrs |
| `GET /eth/v1/node/peers` | Connected peers with status |
| `GET /eth/v1/node/health` | `200` synced, `206` syncing, `503` not ready |
| `GET /eth/v1/node/syncing` | Head block, sync distance, sync status |
| `GET /clique/v1/head` | Current head block (number, hash, signer) |
| `GET /clique/v1/validators` | Current authorized signer set |
| `GET /clique/v1/blocks/{number}` | Block header and metadata |
| `GET /clique/v1/votes` | Pending votes in the current epoch |
| `POST /clique/v1/vote` | Cast a vote to add or remove a signer |

## Documentation

| Document | Description |
|---|---|
| [docs/Architecture.md](docs/Architecture.md) | System design, component descriptions, and data flow |
| [docs/Engine.md](docs/Engine.md) | Engine API integration and JWT authentication |
| [docs/P2P.md](docs/P2P.md) | P2P wire formats, Gossipsub, and status handshake |
| [docs/RPC.md](docs/RPC.md) | JSON-RPC API endpoints with request/response schemas |
| [docs/Docker.md](docs/Docker.md) | Building and running the node as a Docker container |
| [scripts/demo/README.md](scripts/demo/README.md) | Four-node local demo: three signers + one observer, full setup instructions |

## Implementation Status

| Phase | Description | Status |
|---|---|---|
| 1 | Foundation: config, logging, CLI entrypoint | Complete |
| 2 | Engine API client (newPayload, FCU, getPayload, JWT auth) | Complete |
| 3 | Clique consensus engine (EIP-225: signing, verification, snapshots) | Complete |
| 4 | Fork choice (heaviest-chain rule, reorg detection) | Complete |
| 5 | P2P networking (libp2p, Gossipsub, peer discovery) | Complete |
| 6 | JSON-RPC API server | Complete |
| 7 | Block production (turn detection, payload building, broadcast) | Complete |
| 8 | QBFT consensus engine (Istanbul BFT: proposals, prepares, commits, round changes) | Complete |

See [plan.md](plan.md) for the full implementation plan.

## Current Limitations

- **No sync.** There is no mechanism to sync chain history from peers. The node needs to start from a trusted checkpoint or a freshly initialised execution client.
- **No slashing protection.** Equivocation detection (signing two different blocks at the same height) is not implemented.
- **PoA only.** This client is purpose-built for permissioned PoA networks (Clique and QBFT) and is not compatible with Ethereum mainnet (Proof-of-Stake / Gasper).
- **No MEV / builder API.** Block production uses the standard `engine_getPayload` flow; the builder API (mev-boost) is out of scope.

## Project Layout

```
cmd/clique-node/        # Binary entrypoint and CLI flag handling
internal/config/        # TOML configuration loading and validation
internal/log/           # Structured logging (zerolog wrapper)
internal/clique/        # Clique EIP-225 consensus engine
internal/engine/        # Engine API client (JWT auth, newPayload, FCU, getPayload)
internal/p2p/           # libp2p networking, Gossipsub, status handshake
internal/forkchoice/    # Heaviest-chain fork choice store
internal/node/          # Top-level node orchestrator (block processing + production)
internal/rpc/           # JSON-RPC HTTP server
docs/                   # Technical documentation
config.example.toml     # Annotated example configuration
plan.md                 # Full implementation plan
```

## Dependencies

| Package | Purpose |
|---|---|
| `github.com/ethereum/go-ethereum` | Ethereum types, crypto, RLP encoding |
| `github.com/libp2p/go-libp2p` | P2P networking host |
| `github.com/libp2p/go-libp2p-pubsub` | Gossipsub block propagation |
| `github.com/go-chi/chi/v5` | HTTP router for the RPC server |
| `github.com/BurntSushi/toml` | TOML config parsing |
| `github.com/rs/zerolog` | Structured, levelled logging |
| `github.com/golang-jwt/jwt/v5` | JWT tokens for Engine API auth |
| `github.com/urfave/cli/v2` | CLI flag and subcommand handling |
