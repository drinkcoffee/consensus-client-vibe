# Ethereum Clique Consensus Client — Implementation Plan

## Architecture Overview

```
┌───────────────────────────────────────────────────┐
│                   clique-node                     │
│                                                   │
│  ┌───────────┐   ┌──────────┐   ┌──────────────┐  │
│  │  JSON RPC │   │   P2P    │   │  Engine API  │  │
│  │  Server   │   │ (libp2p) │   │  Client (EL) │  │
│  └─────┬─────┘   └────┬─────┘   └──────┬───────┘  │
│        │              │                │          │
│  ┌─────▼──────────────▼────────────────▼───────┐  │
│  │                 Node Core                   │  │
│  │   ┌──────────────┐   ┌────────────────────┐ │  │
│  │   │    Clique    │   │    Fork Choice     │ │  │
│  │   │   Consensus  │   │  (heaviest chain)  │ │  │
│  │   └──────────────┘   └────────────────────┘ │  │
│  └─────────────────────────────────────────────┘  │
└───────────────────────────────────────────────────┘
         │  Engine API (JWT-authenticated HTTP)
         ▼
┌───────────────────────────────────────────────────┐
│     Execution Client (Geth / Nethermind / etc.)   │
└───────────────────────────────────────────────────┘
```

## Project Structure

```
consensus-client-vibe/
├── cmd/
│   └── clique-node/
│       └── main.go                  # Entry point, CLI flags, node startup
├── internal/
│   ├── config/
│   │   └── config.go                # TOML config (p2p, engine, rpc, signer key)
│   ├── clique/
│   │   ├── clique.go                # EIP-225 core: scheduling, difficulty, extra data
│   │   ├── snapshot.go              # Signer set snapshots at checkpoint blocks
│   │   ├── vote.go                  # Vote tallying (add/remove signers)
│   │   └── seal.go                  # Block header sealing (ECDSA sign)
│   ├── engine/
│   │   ├── client.go                # Engine API HTTP client (newPayload, FCU, getPayload)
│   │   ├── jwt.go                   # JWT token generation (HS256, shared secret)
│   │   └── types.go                 # ExecutionPayload, ForkchoiceState, PayloadAttributes
│   ├── p2p/
│   │   ├── host.go                  # libp2p host, transport, security (noise)
│   │   ├── discovery.go             # discv5 peer discovery + mDNS fallback
│   │   ├── gossip.go                # Gossipsub topics: /clique/block/1, /clique/status/1
│   │   ├── handler.go               # Incoming message validation + dispatch
│   │   └── types.go                 # P2P message wire types (SSZ or protobuf)
│   ├── forkchoice/
│   │   └── store.go                 # Head tracking, reorg detection, heaviest-chain rule
│   ├── node/
│   │   ├── node.go                  # Orchestrator: wires all subsystems together
│   │   └── block_processor.go       # Import pipeline: verify → FCU → store
│   └── rpc/
│       ├── server.go                # chi HTTP server, middleware, routes
│       ├── node_handlers.go         # GET /eth/v1/node/{identity,peers,health,syncing}
│       ├── clique_handlers.go       # GET /clique/v1/{head,validators,blocks,votes}
│       └── types.go                 # JSON request/response types
├── config.example.toml
├── plan.md
├── go.mod
└── go.sum
```

## Implementation Phases

### Phase 1 — Foundation
- `go.mod` with module `github.com/peterrobinson/consensus-client-vibe`
- Config loading from TOML (`config.go`) — p2p port, engine URL, JWT secret path, signer key, RPC port
- Structured logging via `zerolog`
- CLI entrypoint with `urfave/cli/v2`

**Key dependencies:** `go-ethereum` (types/crypto), `zerolog`, `urfave/cli/v2`, `BurntSushi/toml`

**Status:** In progress

---

### Phase 2 — Engine API Client
- JWT HS256 token generation (refreshed every 60s) matching EL's shared secret
- HTTP client wrapping `engine_newPayloadV3`, `engine_forkchoiceUpdatedV3`, `engine_getPayloadV3`
- Types: `ExecutionPayloadV3`, `ForkchoiceStateV1`, `PayloadAttributesV3`, `PayloadStatusV1`
- Connection health check on startup

---

### Phase 3 — Clique Consensus Engine
The core of the client — implements EIP-225:

| Concept | Implementation |
|---|---|
| Extra data | 32-byte vanity + [signer list at epoch] + 65-byte sig |
| Signer scheduling | `(blockNum % signerCount) == signerIndex` = in-turn |
| Difficulty | In-turn = 2, out-of-turn = 1 |
| Voting | `coinbase` = vote target, `nonce` = `0xfff...f` (add) or `0x000...0` (remove) |
| Epoch | Every N blocks: checkpoint extra data, reset votes |
| Snapshot | Cached signer set at epoch boundaries, rebuilt by replaying headers |

**Key operations:**
- `VerifyHeader(header)` — recover signer from sig, check turn, check recent signers
- `Seal(header, key)` — sign the header hash, inject into extra data
- `Snapshot(chain, number, hash)` — retrieve or compute signer set at a given block

---

### Phase 4 — Fork Choice
Clique uses a **heaviest-chain** rule (sum of difficulties, like PoW):
- `Store` tracks all known block headers indexed by hash
- `UpdateHead(newBlock)` — compare cumulative difficulty, trigger reorg if needed
- Reorg: walk back to common ancestor, call `engine_forkchoiceUpdated` with new head
- Tracks `headBlock`, `safeBlock` (latest epoch boundary), `finalizedBlock` (2 epochs back)

---

### Phase 5 — P2P Networking (libp2p)
- **Host:** libp2p with TCP transport, Noise security, yamux multiplexing
- **Discovery:** discv5 (using go-ethereum's implementation) + mDNS for local dev
- **Gossipsub topics:**
  - `/clique/block/1` — signed block headers + execution payload hash
  - `/clique/status/1` — peer chain status (head hash, head number, genesis hash)
- **Peer handshake:** exchange Status message on connect; disconnect if incompatible genesis
- **Message types** (SSZ-encoded):
  - `CliqueBlock { Header, ExecutionPayloadHash, Signature }`
  - `Status { GenesisHash, HeadHash, HeadNumber, NetworkID }`

**Key dependencies:** `go-libp2p`, `go-libp2p-pubsub`, `go-libp2p-kad-dht` or discv5

---

### Phase 6 — JSON-RPC API

| Endpoint | Description |
|---|---|
| `GET /eth/v1/node/identity` | libp2p peer ID, ENR, multiaddrs |
| `GET /eth/v1/node/peers` | Connected peers with status |
| `GET /eth/v1/node/health` | 200 if synced, 206 if syncing, 503 if not ready |
| `GET /eth/v1/node/syncing` | `{ head_slot, sync_distance, is_syncing }` |
| `GET /clique/v1/head` | Current head block (number, hash, signer) |
| `GET /clique/v1/validators` | Current authorized signer set |
| `GET /clique/v1/blocks/{number}` | Block header + metadata by number |
| `GET /clique/v1/votes` | Pending votes in current epoch |
| `POST /clique/v1/vote` | Cast a vote (add/remove signer) |

**Key dependencies:** `go-chi/chi`, `go-chi/render`

---

### Phase 7 — Block Production
When it's our signer's turn:
1. Detect turn: `(headNumber+1) % len(signers) == ourIndex`
2. Set a timer: in-turn slot fires immediately, out-of-turn slots add a randomized delay
3. Call `engine_forkchoiceUpdatedV3` with `payloadAttributes` (timestamp, suggested fee recipient, random = prev randao)
4. Poll `engine_getPayloadV3` until payload is ready
5. Build Clique header: set difficulty, extra data vanity; inject vote if desired
6. `Seal(header)` — sign with our key
7. Broadcast via Gossipsub `/clique/block/1`
8. Call `engine_newPayloadV3` to import into EL
9. Call `engine_forkchoiceUpdatedV3` to set as new head

---

## Key Dependencies

```
github.com/ethereum/go-ethereum        # types, crypto, RLP, discv5
github.com/libp2p/go-libp2p            # P2P host
github.com/libp2p/go-libp2p-pubsub    # Gossipsub
github.com/go-chi/chi/v5               # HTTP router
github.com/BurntSushi/toml             # Config
github.com/rs/zerolog                  # Logging
github.com/golang-jwt/jwt/v5           # JWT for Engine API auth
github.com/urfave/cli/v2               # CLI
```

---

## What This Client Does vs. Doesn't Do

**In scope:**
- Clique block validation and production
- Engine API integration with any post-Merge-capable EL
- P2P block propagation between Clique consensus nodes
- REST API for monitoring and validator management

**Out of scope (initially):**
- Sync from genesis (start from trusted checkpoint or paired EL)
- Not needed: Slashing / equivocation detection
- Not needed: MEV / builder API integration
- Not needed: BLS attestations (not applicable to Clique)

**To Check After Initial Build**
- Ensure block information get persisted to disk
- Docker image creation
- Test run with three consensus clients with three Geth execution clients.
- Going from pre-merge network.
- Key storage in KMS.
- Check:
  - Buffer overflows in P2P
  - Is Clique implemented correctly?
  - Are there issues with Clique headers and modern Geth?
  - Are the latest versions of third party libraries being used?
  - Do feature comparison between other consensus client to understand differences
