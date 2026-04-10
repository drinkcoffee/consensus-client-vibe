# Engine API

The Engine API is the JSON-RPC interface between a consensus client and an execution client. It is defined by the [Ethereum execution-apis specification](https://github.com/ethereum/execution-apis/tree/main/src/engine) and is the only channel through which `clique-node` interacts with the EL.

---

## Authentication

All Engine API calls are authenticated with a **JWT Bearer token** in the `Authorization` HTTP header:

```
Authorization: Bearer <token>
```

The token is a signed JWT using the **HS256** algorithm with a 32-byte shared secret. The secret is stored as a hex-encoded file (the same file used by the execution client):

```
# Example jwt.hex
0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef
```

The `0x` prefix is optional. The file must contain exactly 32 bytes (64 hex characters after stripping the prefix).

### Token format

```json
Header:  { "alg": "HS256", "typ": "JWT" }
Payload: { "iat": <unix-timestamp> }
```

The `iat` claim is the Unix timestamp of token creation. Execution clients reject tokens where `|now - iat| > 60 seconds`, so the client refreshes the token 10 seconds before it expires (every 50 seconds in practice).

### Configuration

```toml
[engine]
url             = "http://localhost:8551"
jwt_secret_path = "/path/to/jwt.hex"
```

---

## Methods

### engine_exchangeCapabilities

**When called**: once at node startup, before any other Engine API call.

**Purpose**: negotiate which Engine API versions both sides support. The consensus client sends its list of supported method names; the execution client responds with the intersection it supports. This is used to detect version mismatches early.

**Request**

```json
{
  "jsonrpc": "2.0",
  "method":  "engine_exchangeCapabilities",
  "params":  [["engine_newPayloadV3", "engine_forkchoiceUpdatedV3", "engine_getPayloadV3"]],
  "id":      1
}
```

**Response**

```json
{
  "jsonrpc": "2.0",
  "result":  ["engine_newPayloadV3", "engine_forkchoiceUpdatedV3", "engine_getPayloadV3"],
  "id":      1
}
```

---

### engine_newPayloadV3

**When called**:
1. When a block is **received from a peer** over Gossipsub and passes Clique consensus verification.
2. When this node **produces a block**: after building and sealing the Clique header, to import the execution payload into the local EL.

**Purpose**: submit an execution payload to the EL for validation and execution. The EL verifies the payload (state root, receipts root, gas usage, etc.) and reports whether it is valid.

**Request params**: `[ExecutionPayloadV3, versioned_hashes, parent_beacon_block_root]`

```json
{
  "parentHash":    "0x...",
  "feeRecipient":  "0x...",
  "stateRoot":     "0x...",
  "receiptsRoot":  "0x...",
  "logsBloom":     "0x...",
  "prevRandao":    "0x...",
  "blockNumber":   "0x1a",
  "gasLimit":      "0x1c9c380",
  "gasUsed":       "0x5208",
  "timestamp":     "0x65e1c000",
  "extraData":     "0x",
  "baseFeePerGas": "0x7",
  "blockHash":     "0x...",
  "transactions":  ["0x..."],
  "withdrawals":   [],
  "blobGasUsed":   "0x0",
  "excessBlobGas": "0x0"
}
```

**Response** (`PayloadStatusV1`)

```json
{
  "status":          "VALID",
  "latestValidHash": "0x...",
  "validationError": null
}
```

| Status | Meaning |
|---|---|
| `VALID` | Payload is valid; EL has executed it |
| `INVALID` | Payload is invalid (bad state root, etc.); `validationError` is populated |
| `SYNCING` | EL has not yet verified the payload's parent; asking CL to wait |
| `ACCEPTED` | Payload is syntactically valid but not yet validated (optimistic processing) |
| `INVALID_BLOCK_HASH` | The `blockHash` field does not match the computed hash |

**On `INVALID`**: the block is discarded. If it was from a peer, the peer is not currently penalised (future: peer scoring).

**On `SYNCING`**: the block is queued for reprocessing once the EL catches up.

---

### engine_forkchoiceUpdatedV3

**When called**:
1. When the **canonical head changes** (new highest-TD block accepted).
2. When this node **produces a block**: first to request a new payload be built (with `payloadAttributes`), then again to set the produced block as the new head (without `payloadAttributes`).

**Purpose**: inform the EL of the current canonical head, safe block, and finalized block. Optionally request the EL to begin building a new execution payload.

**Request params**: `[ForkchoiceStateV1, PayloadAttributesV3 | null]`

```json
[
  {
    "headBlockHash":      "0x...",
    "safeBlockHash":      "0x...",
    "finalizedBlockHash": "0x..."
  },
  {
    "timestamp":             "0x65e1c03c",
    "prevRandao":            "0x0000000000000000000000000000000000000000000000000000000000000000",
    "suggestedFeeRecipient": "0x1111111111111111111111111111111111111111",
    "withdrawals":           [],
    "parentBeaconBlockRoot": "0x0000000000000000000000000000000000000000000000000000000000000000"
  }
]
```

**Fork choice state fields**

| Field | Description |
|---|---|
| `headBlockHash` | Hash of the canonical chain tip (highest TD block) |
| `safeBlockHash` | Hash of the most recent epoch-boundary block (Clique checkpoint) |
| `finalizedBlockHash` | Hash of the epoch-boundary block two epochs before the head |

**Payload attributes** (only when requesting a new payload)

| Field | Description |
|---|---|
| `timestamp` | Target timestamp for the new block (`parent.timestamp + clique.period`) |
| `prevRandao` | Set to `0x00…0` for Clique (no beacon chain randomness) |
| `suggestedFeeRecipient` | The signer's address (fee recipient for produced blocks) |
| `withdrawals` | Empty for Clique networks |
| `parentBeaconBlockRoot` | Set to `0x00…0` for Clique |

**Response** (`ForkchoiceUpdatedResult`)

```json
{
  "payloadStatus": {
    "status":          "VALID",
    "latestValidHash": "0x...",
    "validationError": null
  },
  "payloadId": "0x0000000000000001"
}
```

`payloadId` is non-null only when `payloadAttributes` was provided. It is used in the subsequent `engine_getPayloadV3` call.

---

### engine_getPayloadV3

**When called**: during **block production**, after calling `engine_forkchoiceUpdatedV3` with `payloadAttributes` to start payload building.

**Purpose**: retrieve the execution payload the EL has built. The EL selects the best available transactions from the mempool, executes them, and returns the resulting execution payload.

**Request params**: `[payloadId]`

```json
["0x0000000000000001"]
```

**Response** (`GetPayloadResponseV3`)

```json
{
  "executionPayload": {
    "parentHash":    "0x...",
    "feeRecipient":  "0x...",
    "stateRoot":     "0x...",
    "receiptsRoot":  "0x...",
    "logsBloom":     "0x...",
    "prevRandao":    "0x...",
    "blockNumber":   "0x1a",
    "gasLimit":      "0x1c9c380",
    "gasUsed":       "0x5208",
    "timestamp":     "0x65e1c03c",
    "extraData":     "0x",
    "baseFeePerGas": "0x7",
    "blockHash":     "0x...",
    "transactions":  ["0x..."],
    "withdrawals":   [],
    "blobGasUsed":   "0x0",
    "excessBlobGas": "0x0"
  },
  "blockValue": "0x0",
  "blobsBundle": {
    "commitments": [],
    "proofs":      [],
    "blobs":       []
  }
}
```

The `executionPayload.blockHash` from this response is stored as `CliqueBlock.ExecutionPayloadHash` when the block is broadcast to peers.

---

## Call Sequence: Receiving a Block

```
1. engine_newPayloadV3(payload)
        → VALID / SYNCING / INVALID

2. (if VALID and new head)
   engine_forkchoiceUpdatedV3(state, nil)
        → VALID
```

---

## Call Sequence: Producing a Block

```
1. engine_forkchoiceUpdatedV3(currentState, payloadAttributes)
        → { payloadStatus: VALID, payloadId: "0x..." }

2. engine_getPayloadV3(payloadId)
        → { executionPayload, blockValue, blobsBundle }

3. Build + seal Clique header

4. Broadcast CliqueBlock over Gossipsub

5. engine_newPayloadV3(executionPayload)
        → VALID  (importing into own EL)

6. engine_forkchoiceUpdatedV3(newState, nil)
        → VALID  (set produced block as new head)
```

---

## Error Handling

| Situation | Behaviour |
|---|---|
| EL unreachable at startup | Node exits with a fatal error |
| JSON-RPC error response | The error message is logged; the operation is aborted |
| `newPayload` → `INVALID` | Block discarded |
| `newPayload` → `SYNCING` | Block queued for retry (future implementation) |
| `forkchoiceUpdated` → any non-VALID status | Logged as a warning |
| Request timeout | Controlled by `engine.call_timeout` (default 5 s) |

---

## Configuration Reference

```toml
[engine]
# Engine API endpoint of the execution client.
url = "http://localhost:8551"

# Path to the hex-encoded 32-byte JWT shared secret.
jwt_secret_path = "/path/to/jwt.hex"

# How long to wait for the initial connection to the EL.
dial_timeout = "10s"

# Per-request timeout for Engine API calls.
call_timeout = "5s"
```
