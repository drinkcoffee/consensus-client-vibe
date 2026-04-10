# consensus-client-vibe

An Ethereum consensus client written in Go that uses [Clique](https://eips.ethereum.org/EIPS/eip-225) (Proof-of-Authority) consensus. It communicates with any Ethereum execution client via the [Engine API](https://github.com/ethereum/execution-apis/tree/main/src/engine), propagates blocks over a libp2p P2P network, and exposes a JSON-RPC HTTP API for node monitoring and validator management.

This is a vibe-coded project — architecture-first, iteratively built.

## What It Is

Most Ethereum consensus clients (Lighthouse, Prysm, Teku, Nimbus) implement Proof-of-Stake (Gasper). This client instead implements **Clique PoA**, where a fixed set of authorized signers take turns producing blocks. This makes it suitable for:

- Private/permissioned Ethereum networks
- Local development chains (replacing the Clique engine embedded in Geth)
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

Key settings:

| Section | Field | Description |
|---|---|---|
| `[node]` | `network_id` | Must match your execution client's chain/network ID |
| `[engine]` | `url` | Engine API endpoint of your execution client (default `:8551`) |
| `[engine]` | `jwt_secret_path` | Path to the shared JWT secret file (hex-encoded) |
| `[engine]` | `el_rpc_url` | Regular JSON-RPC endpoint used to fetch the genesis block on startup (default `http://localhost:8545`) |
| `[clique]` | `signer_key_path` | ECDSA private key for signing blocks; omit for follower mode |
| `[clique]` | `period` | Seconds between blocks — must match `genesis.config.clique.period` |
| `[clique]` | `epoch` | Blocks per epoch — must match `genesis.config.clique.epoch` |
| `[p2p]` | `listen_addr` | libp2p multiaddr to listen on, e.g. `/ip4/0.0.0.0/tcp/9000` |
| `[rpc]` | `listen_addr` | JSON-RPC HTTP server address, e.g. `0.0.0.0:5052` |

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

See [plan.md](plan.md) for the full implementation plan.

## Current Limitations

- **No sync.** There is no mechanism to sync chain history from peers. The node needs to start from a trusted checkpoint or a freshly initialised execution client.
- **No slashing protection.** Equivocation detection (signing two different blocks at the same height) is not implemented.
- **Clique only.** This client is purpose-built for Clique PoA and is not compatible with Ethereum mainnet (Proof-of-Stake / Gasper).
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
