# Architecture

## Overview

`consensus-client-vibe` is a standalone Ethereum consensus client that implements the [Clique Proof-of-Authority consensus algorithm (EIP-225)](https://eips.ethereum.org/EIPS/eip-225). It follows the post-Merge split architecture where a **consensus client** (this program) and an **execution client** (Geth, Nethermind, etc.) run as separate processes and communicate over the Engine API.

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
         │  Engine API (JWT-authenticated HTTP/JSON-RPC)
         ▼
┌───────────────────────────────────────────────────┐
│     Execution Client (Geth / Nethermind / etc.)   │
└───────────────────────────────────────────────────┘
```

## The Consensus/Execution Split

After Ethereum's Merge, responsibility was divided between two separate processes:

| Responsibility | Execution Client | Consensus Client (this project) |
|---|---|---|
| Transaction pool | ✓ | |
| EVM execution | ✓ | |
| State trie | ✓ | |
| Block ordering | | ✓ |
| Fork choice | | ✓ |
| Block signing | | ✓ |
| Peer discovery (chain) | | ✓ |

The two clients communicate exclusively over the **Engine API** — a JWT-authenticated JSON-RPC interface over HTTP.

## Components

### Node Core (`internal/node/`)

The top-level orchestrator. On startup it:

1. Loads configuration and initialises all subsystems.
2. Connects to the execution client via the Engine API (health-checked on startup).
3. Starts P2P networking and joins the Gossipsub block topic.
4. Starts the JSON-RPC HTTP server.
5. Enters the main loop: receive blocks from P2P, verify them, update fork choice, call `engine_forkchoiceUpdated`. When it is this node's turn to produce a block, drives the block production flow.

### Clique Consensus Engine (`internal/clique/`)

Pure consensus logic with no network I/O. Implements EIP-225:

| Concept | Detail |
|---|---|
| **Extra data** | 32-byte vanity ‖ [signer list at epoch boundaries] ‖ 65-byte ECDSA seal |
| **Signer scheduling** | In-turn signer at block N: `signerList[N % len(signers)]` |
| **Difficulty** | In-turn = 2, out-of-turn = 1 |
| **Voting** | Coinbase = vote target; nonce `0xfff…f` = add, `0x000…0` = remove |
| **Epoch** | Every `epoch` blocks: checkpoint with signer list embedded in extra data; pending votes reset |
| **Snapshot** | The authorised signer set at a given block, derived by replaying headers from the last epoch checkpoint |

Key operations:
- `VerifyHeader(snap, header, parent)` — verifies extra data, nonce, mix digest, uncle hash, difficulty, timestamp, signer authorisation, and recency limit.
- `Apply(snap, headers)` — advances a snapshot forward by replaying a sequence of headers (handles voting and epoch resets).
- `SealHeader(header, key)` — signs the header hash and injects the 65-byte signature into the trailing bytes of `Extra`.
- `CalcDifficulty(snap, number, signer)` — returns 2 (in-turn) or 1 (out-of-turn).

The `Snapshot` type is the central state object. It carries:
- `Signers` — the current authorised signer set.
- `Recents` — recent signers (used to enforce the "must wait" rule: a signer cannot sign again within `floor(len(signers)/2)+1` blocks).
- `Votes` / `Tally` — pending vote tallies within the current epoch.

### Fork Choice (`internal/forkchoice/`)

Clique uses the **heaviest-chain rule**: the canonical chain is the one with the highest cumulative difficulty (sum of block difficulties), identical to pre-Merge proof-of-work. This means:

- A chain of N in-turn blocks (difficulty 2 each) outweighs a chain of N out-of-turn blocks (difficulty 1 each).
- Reorgs are resolved by comparing total difficulty at the competing tips.

The `Store` maintains:
- All known block headers indexed by hash.
- A canonical-chain index (block number → hash).
- Three chain tip pointers required by the Engine API:
  - **Head** — the tip of the heaviest known chain.
  - **Safe** — the most recent epoch-boundary block on the canonical chain.
  - **Finalized** — the epoch-boundary block two epochs before the head (treated as irreversible for practical purposes).

### Engine API Client (`internal/engine/`)

Wraps the Engine API with:
- JWT HS256 token generation (refreshed every 60 s using a 32-byte shared secret).
- `engine_newPayloadV3` — submit a new execution payload for validation.
- `engine_forkchoiceUpdatedV3` — update the fork choice state; optionally request a new payload be built.
- `engine_getPayloadV3` — retrieve a completed payload from the execution client.
- `engine_exchangeCapabilities` — handshake to confirm supported API methods.

See [Engine.md](Engine.md) for full details.

### P2P Networking (`internal/p2p/`)

Built on [libp2p](https://libp2p.io/):
- **Transport**: TCP with Noise (+ TLS) security and yamux stream multiplexing.
- **Block propagation**: Gossipsub on topic `/clique/block/1`.
- **Peer handshake**: Stream protocol `/clique/status/1` — peers exchange `StatusMsg` on each new outbound connection; incompatible genesis or network ID results in disconnection.
- **Discovery**: mDNS for local networks; static bootnodes for production.

See [P2P.md](P2P.md) for full details.

### JSON-RPC Server (`internal/rpc/`)

A chi-based HTTP server exposing Ethereum Beacon Node-compatible endpoints (`/eth/v1/node/…`) and Clique-specific endpoints (`/clique/v1/…`). Responses follow the `{"data": …}` envelope convention.

See [RPC.md](RPC.md) for full details.

## Package Layout

```
cmd/
  clique-node/
    main.go                # CLI entrypoint, flag parsing, node startup
internal/
  config/
    config.go              # TOML config loading and validation
  log/
    log.go                 # zerolog wrapper with component tags
  clique/
    clique.go              # Engine: VerifyHeader, CalcDifficulty, Apply
    snapshot.go            # Snapshot type: signer set, recents, votes
    vote.go                # Vote and Tally types
    seal.go                # sigHash, SealHeader, SignerFromHeader
  engine/
    client.go              # Engine API JSON-RPC client
    jwt.go                 # JWT HS256 token provider
    types.go               # ExecutionPayloadV3, ForkchoiceStateV1, etc.
  p2p/
    host.go                # libp2p host, Gossipsub, status protocol
    types.go               # CliqueBlock and StatusMsg wire types
    discovery.go           # mDNS peer discovery
  forkchoice/
    store.go               # Heaviest-chain store with reorg detection
  rpc/
    server.go              # chi HTTP server, routing, lifecycle
    node_handlers.go       # /eth/v1/node/* handlers
    clique_handlers.go     # /clique/v1/* handlers
    types.go               # Interfaces and JSON types
    helpers.go             # Address utilities
  node/                    # (Phase 7) Top-level orchestrator
    node.go
    block_processor.go
config.example.toml        # Annotated example configuration
plan.md                    # Original implementation plan
docs/                      # This documentation
```

## Data Flow: Receiving a Block

```
Gossipsub /clique/block/1
        │
        ▼
  p2p.Host.subscribeBlocks()
        │  DecodeCliqueBlock → *CliqueBlock
        │
        ▼
  node.blockProcessor.HandleBlock()
        │  clique.SignerFromHeader → signer address
        │  engine.VerifyHeader(snap, header, parent)
        │
        ▼
  engine.NewPayloadV3(ctx, payload)   ← sends to EL
        │  validate / execute
        │
        ▼
  forkchoice.Store.AddBlock(header)
        │  compute TD, detect reorg
        │  returns headChanged bool
        │
        ▼  (if headChanged)
  engine.ForkchoiceUpdatedV3(ctx, state, nil)
        │  set EL canonical head
        │
        ▼
  rpc snapshot updated
```

## Data Flow: Producing a Block

```
Timer fires: it is our signer's turn at block N+1
        │
        ▼
  node.blockProducer.Tick()
        │  engine.ForkchoiceUpdatedV3(ctx, state, payloadAttributes)
        │  → returns PayloadID
        │
        ▼
  engine.GetPayloadV3(ctx, payloadID)
        │  → ExecutionPayloadV3 + BlobsBundle
        │
        ▼
  Build Clique header
        │  set Difficulty, Extra (vanity + optional vote)
        │  clique.SealHeader(header, signerKey)
        │
        ▼
  p2p.Host.BroadcastBlock(ctx, block)   ← gossip to peers
        │
        ▼
  engine.NewPayloadV3(ctx, payload)     ← import into own EL
        │
        ▼
  engine.ForkchoiceUpdatedV3(ctx, state, nil)   ← set as head
```

## Security Considerations

- **JWT secret** must be kept private and must match the execution client's secret. The file should be `0600` and the path should not be world-readable.
- **Signer key** is a raw ECDSA private key. Losing it means the node can no longer sign blocks. Compromising it allows an attacker to sign blocks on behalf of this node. KMS-backed key storage is a future improvement.
- **No equivocation protection**: the node does not currently detect or prevent double-signing (signing two different blocks at the same height). This is safe in practice only when the signer key is held by a single node.
- **P2P is unauthenticated**: any peer can connect and send messages. Message validation (signer recovery, difficulty check) happens before blocks are accepted into the store.
