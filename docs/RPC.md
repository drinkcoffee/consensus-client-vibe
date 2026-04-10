# JSON-RPC API

The node exposes an HTTP server with two groups of endpoints:

- `/eth/v1/node/…` — Ethereum Beacon Node-compatible endpoints for general node monitoring.
- `/clique/v1/…` — Clique-specific endpoints for chain inspection and validator management.

All responses use JSON. Successful responses wrap their payload in a `{"data": …}` envelope. Error responses use `{"code": <http-status>, "message": "<description>"}`.

The listen address is configured via `rpc.listen_addr` (default `0.0.0.0:5052`).

---

## Node Endpoints

### GET /eth/v1/node/identity

Returns the local node's libp2p peer identity and listen addresses.

**Response**

```json
{
  "data": {
    "peer_id": "12D3KooWGatU9DEesagYvh8aVym6BBt7SDUDC2pwAK1zppS2HDYr",
    "enr": "",
    "p2p_addresses": [
      "/ip4/0.0.0.0/tcp/9000/p2p/12D3KooWGatU9DEesagYvh8aVym6BBt7SDUDC2pwAK1zppS2HDYr"
    ]
  }
}
```

| Field | Description |
|---|---|
| `peer_id` | Base58-encoded libp2p peer ID (derived from the node's private key) |
| `enr` | Ethereum Node Record — empty until discv5 is implemented |
| `p2p_addresses` | Full multiaddrs including the `/p2p/<peer-id>` component |

---

### GET /eth/v1/node/peers

Returns all currently connected peers.

**Response**

```json
{
  "data": [
    {
      "peer_id": "12D3KooWMZVe9t3AtUp3P8vjEycXf4p9Hnq1dZx2CX1dFkWiYooh",
      "address": "/ip4/192.0.2.1/tcp/9000/p2p/12D3KooWMZVe9t3AtUp3P8vjEycXf4p9Hnq1dZx2CX1dFkWiYooh",
      "direction": "unknown",
      "state": "connected"
    }
  ],
  "meta": {
    "count": 1
  }
}
```

| Field | Description |
|---|---|
| `peer_id` | Remote peer's libp2p ID |
| `address` | Remote peer's multiaddr (first known address) |
| `direction` | `"inbound"` / `"outbound"` / `"unknown"` — currently always `"unknown"` |
| `state` | Always `"connected"` (disconnected peers are not listed) |
| `meta.count` | Total number of connected peers |

---

### GET /eth/v1/node/health

Reports whether the node is ready to serve requests.

**Status codes**

| Code | Meaning |
|---|---|
| `200 OK` | Node has a head block and at least one connected peer |
| `503 Service Unavailable` | Node has no head block or no connected peers |

No response body is returned; callers should inspect the status code only.

---

### GET /eth/v1/node/syncing

Returns the current sync status.

**Response**

```json
{
  "data": {
    "head_slot": "1042",
    "sync_distance": "0",
    "is_syncing": false
  }
}
```

| Field | Description |
|---|---|
| `head_slot` | Current canonical head block number (Clique uses block numbers, not slots) |
| `sync_distance` | Estimated distance to the network head — `"0"` until sync tracking is implemented |
| `is_syncing` | `false` until active sync is implemented |

---

## Clique Endpoints

### GET /clique/v1/head

Returns details about the current canonical chain tip.

**Response**

```json
{
  "data": {
    "number": "1042",
    "hash": "0xabcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890",
    "signer": "0x1111111111111111111111111111111111111111",
    "timestamp": "1712345678",
    "total_difficulty": "2084"
  }
}
```

| Field | Description |
|---|---|
| `number` | Canonical head block number (decimal string) |
| `hash` | Canonical head block hash (0x-prefixed hex) |
| `signer` | Address of the signer who produced this block, recovered from the header seal |
| `timestamp` | Unix timestamp of the block (decimal string) |
| `total_difficulty` | Cumulative difficulty of the canonical chain (decimal string) |

**Error responses**

| Code | Condition |
|---|---|
| `503` | No head block is available (node not yet initialised) |

---

### GET /clique/v1/validators

Returns the current authorised signer set derived from the head snapshot.

**Response**

```json
{
  "data": {
    "signers": [
      "0x1111111111111111111111111111111111111111",
      "0x2222222222222222222222222222222222222222",
      "0x3333333333333333333333333333333333333333"
    ],
    "count": 3
  }
}
```

Signers are sorted lexicographically by address bytes (the canonical Clique ordering). This order determines the in-turn schedule: signer at index `N % len(signers)` is in-turn for block `N`.

If no snapshot is available yet, returns `{"data": {"signers": [], "count": 0}}`.

---

### GET /clique/v1/blocks/{number}

Returns header metadata for the canonical block at the given block number.

**Path parameter**

| Parameter | Type | Description |
|---|---|---|
| `number` | uint64 | Decimal block number |

**Response**

```json
{
  "data": {
    "number": "42",
    "hash": "0xabcdef...",
    "parent_hash": "0x123456...",
    "timestamp": "1712345000",
    "difficulty": "2",
    "signer": "0x1111111111111111111111111111111111111111",
    "extra": "0xd883010e06846765746888676f312e32332e30856c696e75780000000000000000..."
  }
}
```

| Field | Description |
|---|---|
| `number` | Block number (decimal string) |
| `hash` | Block hash |
| `parent_hash` | Parent block hash |
| `timestamp` | Unix timestamp |
| `difficulty` | `"2"` for in-turn blocks, `"1"` for out-of-turn blocks |
| `signer` | Address recovered from the 65-byte seal in `extra` |
| `extra` | Full extra data field (0x-prefixed hex): 32-byte vanity + optional signer list at epoch boundaries + 65-byte seal |

**Error responses**

| Code | Condition |
|---|---|
| `400` | `number` is not a valid decimal integer |
| `404` | Block not in the canonical chain store |

---

### GET /clique/v1/votes

Returns all pending votes in the current epoch from the head snapshot.

**Response**

```json
{
  "data": [
    {
      "signer": "0x1111111111111111111111111111111111111111",
      "address": "0x4444444444444444444444444444444444444444",
      "authorize": true,
      "block": "38"
    },
    {
      "signer": "0x2222222222222222222222222222222222222222",
      "address": "0x4444444444444444444444444444444444444444",
      "authorize": true,
      "block": "41"
    }
  ]
}
```

| Field | Description |
|---|---|
| `signer` | The authorised signer who cast this vote |
| `address` | The address being voted for (candidate to add or remove) |
| `authorize` | `true` to add the address, `false` to remove it |
| `block` | Block number at which this vote was cast |

Votes are cleared at every epoch boundary. Returns an empty array if no snapshot is available.

---

### POST /clique/v1/vote

Stores a vote intent to be included in the next block this node produces. The vote is embedded in the produced block's `coinbase` (target address) and `nonce` fields according to EIP-225.

**Request body**

```json
{
  "address": "0x4444444444444444444444444444444444444444",
  "authorize": true
}
```

| Field | Type | Description |
|---|---|---|
| `address` | string | 0x-prefixed 20-byte hex address of the vote target |
| `authorize` | bool | `true` to vote to add the address; `false` to vote to remove it |

**Response**

```json
{
  "data": {
    "status": "pending vote set"
  }
}
```

Only one pending vote is stored at a time. Calling this endpoint again replaces the previous pending vote. The vote is consumed (cleared) when the node produces a block.

**Error responses**

| Code | Condition |
|---|---|
| `400` | Request body is not valid JSON |
| `400` | `address` is not a valid 0x-prefixed 20-byte hex address |
| `400` | `address` is the zero address (`0x0000…0000`) |

---

## Error Response Format

All error responses use the following body:

```json
{
  "code": 404,
  "message": "block 9999 not found"
}
```

The `code` field mirrors the HTTP status code.

---

## Notes

- **Content-Type**: all responses set `Content-Type: application/json`.
- **Numbers as strings**: block numbers, timestamps, difficulty, and total difficulty are returned as decimal strings to avoid JavaScript integer overflow with large values.
- **No authentication**: the RPC server does not currently require authentication. Bind it to a loopback address (`127.0.0.1`) or protect it with a reverse proxy in production.
