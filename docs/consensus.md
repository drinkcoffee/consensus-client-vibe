# Consensus Block Format

This document explains how `consensus-client-vibe` represents and processes
Clique blocks across the consensus layer (CL) and execution layer (EL), and why
the two layers maintain separate block hashes.

## Background: the CL/EL Split

Since the Merge, Ethereum separates block production into two layers:

- The **execution layer** (EL) — Geth, Nethermind, etc. — handles the EVM,
  transaction processing, and the block body (transactions, receipts, state).
- The **consensus layer** (CL) — this client — drives block ordering, fork
  choice, and the consensus protocol (Clique in this case).

The two layers communicate through the [Engine API](Engine.md) using
JWT-authenticated JSON-RPC. The CL tells the EL which block to build next
(`engine_forkchoiceUpdatedV3` with `PayloadAttributes`), fetches the built
payload (`engine_getPayloadV3`), and tells the EL to import and accept the
payload (`engine_newPayloadV3`, then another `engine_forkchoiceUpdatedV3`).

## The extraData Conflict

[EIP-225 (Clique)](https://eips.ethereum.org/EIPS/eip-225) encodes the
consensus signature directly in the EL block header's `extraData` field:

```
extraData = [32-byte vanity] [N × 20-byte signer addresses (epoch only)] [65-byte ECDSA seal]
```

For a non-epoch block with one signer, this is 97 bytes.

Post-merge Geth operating in beacon-client mode enforces a **maximum of 32
bytes** on `extraData`. This is intentional: in the original Ethereum PoS
design the consensus layer (a beacon chain block) carries the validator
signature, and the EL block's `extraData` is a free field used only for miner
messages. The 32-byte cap keeps the EL payload format clean.

Supplying a 97-byte `extraData` to `engine_newPayloadV3` produces:

```
status=INVALID error=invalid extradata length: 97
```

## The Solution: Two Separate Hash Spaces

The fix mirrors what Ethereum PoS already does: **the signature lives in the CL
block, not in the EL block**.

```
┌─────────────────────────────────────────────────────────┐
│  CL block (CliqueBlock, propagated over Gossipsub)        │
│                                                           │
│  Header.Extra = [32B vanity][N×20B signers][65B seal]     │
│  CL hash = keccak256(RLP(Header))                         │
│  ExecutionPayloadHash ──────────────────────────────────┐ │
└─────────────────────────────────────────────────────────┼─┘
                                                          │
                        ┌─────────────────────────────────▼──┐
                        │  EL block (managed by Geth)         │
                        │                                     │
                        │  extraData = [32B vanity only]      │
                        │  EL hash = keccak256(RLP(ELHeader)) │
                        └─────────────────────────────────────┘
```

Each block therefore has **two hashes**:

| Hash | Contains | Used for |
|---|---|---|
| **CL hash** | Full Clique `Extra` with seal | Signer recovery, snapshot computation, CL parent-chain lookups |
| **EL hash** | 32-byte `extraData` only | `engine_forkchoiceUpdated`, `engine_newPayload`, devp2p |

The CL block carries the `ExecutionPayloadHash` field so any node that receives
a CL block over Gossipsub knows the corresponding EL block hash to supply to
its execution client.

## Implementation

### Block Production (`internal/node/block_producer.go`)

When producing a block, the node:

1. Builds two `extraData` values:
   - `elExtra` — 32 zero bytes (the EL vanity field, accepted by Geth)
   - `clExtra` — full Clique format: vanity + optional epoch signers + 65-byte
     seal placeholder (built by `buildExtra`)

2. Calls `engine_forkchoiceUpdatedV3` with `PayloadAttributes.ExtraData = elExtra`.
   Geth builds an EL block with a 32-byte `extraData` and returns the EL
   payload hash (`ep.BlockHash`).

3. Constructs the CL `*types.Header` with `Extra = clExtra`, copies the state
   root, receipts root, and other fields from the EL payload.

4. Seals the CL header with the signer ECDSA key. The seal is written into
   `header.Extra`; the CL hash (`header.Hash()`) now differs from `ep.BlockHash`.

5. Does **not** modify `ep.ExtraData` or `ep.BlockHash`. The EL payload is
   imported into Geth unchanged.

6. Calls `NewCliqueBlock(header, ep.BlockHash)` to create the Gossipsub
   message, linking the CL header to the EL block hash.

7. Calls `stor.AddBlock(header, ep.BlockHash)` to record both hashes in the
   fork-choice store.

### Block Processing (`internal/node/block_processor.go`)

When a block arrives over Gossipsub:

1. The CL header is decoded and verified against the Clique rules (signer
   recovery uses the full 97-byte `Extra`).

2. `stor.AddBlock(header, blk.ExecutionPayloadHash)` records the CL header
   keyed by its CL hash, and maps that CL hash to the EL payload hash.

3. If the block extends the canonical chain, `stor.ForkchoiceState()` is called
   and the result is passed to `engine_forkchoiceUpdatedV3`. The state contains
   **EL hashes**, so Geth updates its own chain tip correctly.

### Fork Choice Store (`internal/forkchoice/store.go`)

The store maintains:

- `blocks map[CL hash → blockEntry]` — all known CL headers
- `elHashes map[CL hash → EL hash]` — the EL payload hash for each CL block
- `numbers map[block number → CL hash]` — canonical chain index

`ForkchoiceState()` looks up the EL hash for the head, safe, and finalized
pointers before returning the `ForkchoiceStateV1` struct. All three hashes
supplied to the Engine API are EL hashes.

For the genesis block the CL hash and EL hash are identical because genesis is
initialised by the EL (`geth init`) and has no Clique seal; the CL reads the
genesis header directly from the EL via `eth_getBlockByNumber("0x0")`.

### P2P Wire Format (`internal/p2p/types.go`)

```go
type CliqueBlock struct {
    Header               rlp.RawValue // RLP-encoded *types.Header (full Clique Extra)
    ExecutionPayloadHash common.Hash  // EL block hash
}
```

The `Header` field encodes the CL header so receiving nodes can recover the
signer, verify the seal, and apply the Clique snapshot rules. The
`ExecutionPayloadHash` is the EL block hash that the receiving node must pass to
`engine_forkchoiceUpdated` to advance its own execution client.
