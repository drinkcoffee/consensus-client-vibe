# P2P Networking

The P2P layer is built on [libp2p](https://libp2p.io/) and uses two protocols:

| Protocol | Transport | Purpose |
|---|---|---|
| `/clique/block/1` | Gossipsub | Propagate signed block headers to all peers |
| `/clique/status/1` | libp2p stream | Exchange chain state on peer connection |

---

## Transport Stack

```
Application (CliqueBlock / StatusMsg)
     │
     ▼
  RLP encoding
     │
     ▼
  libp2p stream / Gossipsub
     │
     ▼
  yamux (stream multiplexer)
     │
     ▼
  Noise + TLS (transport security)
     │
     ▼
  TCP
```

- **Security**: Noise protocol (with TLS fallback) — both are enabled by default in libp2p.
- **Multiplexer**: yamux — multiple logical streams over a single TCP connection.
- **Peer identity**: Ed25519 key pair generated per node startup (not yet persisted to disk).

---

## Message Encoding

### Gossipsub messages

Messages published on Gossipsub topics are **RLP-encoded** structs. No additional framing is needed; the Gossipsub layer provides message boundaries.

### Stream messages (status protocol)

Messages sent over the `/clique/status/1` stream use **length-prefixed RLP**:

```
┌───────────────────────────────────────────────────┐
│  Length (4 bytes, big-endian uint32)              │
├───────────────────────────────────────────────────┤
│  RLP-encoded payload (Length bytes)               │
└───────────────────────────────────────────────────┘
```

Maximum message size for status messages: **1 KB**.

---

## Messages

### CliqueBlock

Carried on Gossipsub topic `/clique/block/1`. Propagates a signed Clique block header together with the hash of the corresponding execution payload.

**Wire type** (RLP-encoded struct):

```
CliqueBlock {
    Header               RawValue  // RLP-encoded *types.Header (includes 65-byte seal)
    ExecutionPayloadHash Hash      // keccak256 of the execution payload body
}
```

| Field | Size | Description |
|---|---|---|
| `Header` | variable | Complete go-ethereum `types.Header` encoded as RLP. The last 65 bytes of `Extra` are the ECDSA seal over `sigHash(header)`. |
| `ExecutionPayloadHash` | 32 bytes | Hash of the execution payload managed by the paired EL. Used to correlate the consensus header with the EL's block. |

**When sent:**

1. When this node **produces** a block: after sealing the header, before calling `engine_newPayloadV3` on the local EL.
2. When this node **receives** a valid block from the EL that it did not originate: after verification, re-broadcast to ensure full propagation (future: not yet implemented).

**Processing on receipt:**

1. Decode the `CliqueBlock` from the Gossipsub message.
2. Recover the signer from the header seal (`clique.SignerFromHeader`).
3. Verify the header against the current snapshot (`clique.Engine.VerifyHeader`).
4. Submit the execution payload to the EL (`engine_newPayloadV3`).
5. Add the header to the fork choice store (`forkchoice.Store.AddBlock`).
6. If the new block becomes the canonical head, call `engine_forkchoiceUpdatedV3`.

**Self-message filtering:** a node's own published messages are received back by the local Gossipsub subscription and are silently dropped (checked via `msg.ReceivedFrom == host.ID()`).

---

### StatusMsg

Carried on stream protocol `/clique/status/1`. Exchanged once per connection to verify that both peers are on the same network and chain.

**Wire type** (length-prefixed RLP):

```
StatusMsg {
    NetworkID   uint64  // must match node.network_id config
    GenesisHash Hash    // keccak256 hash of the genesis block header
    HeadHash    Hash    // keccak256 hash of the current canonical head
    HeadNumber  uint64  // block number of the current canonical head
}
```

| Field | Size | Description |
|---|---|---|
| `NetworkID` | 8 bytes | Network identifier. Must match between peers or the connection is dropped. |
| `GenesisHash` | 32 bytes | Hash of block 0. Must match between peers or the connection is dropped. |
| `HeadHash` | 32 bytes | Current canonical head hash. Informational — used for logging and future sync. |
| `HeadNumber` | 8 bytes | Current canonical head block number. Informational. |

**Handshake flow:**

Only the **dialing** (outbound) side initiates the handshake to avoid both peers opening a status stream simultaneously.

```
Dialer (outbound)                  Listener (inbound)
      │                                   │
      │── open /clique/status/1 ──────────▶│
      │                                   │  SetStreamHandler registered
      │── StatusMsg (own status) ─────────▶│
      │                                   │  validate + process
      │◀── StatusMsg (peer status) ────────│
      │                                   │
      │  validate + process               │
      │── close stream ──────────────────▶│
```

**Incompatibility handling:**

If `NetworkID` or `GenesisHash` does not match:
- A warning is logged with both values.
- `network.ClosePeer(pid)` is called to terminate the connection.

**Status updates:**

The local `StatusMsg` is updated via `Host.SetStatus` whenever the canonical head changes. Subsequent connections will include the updated head hash and number.

---

## Peer Discovery

### mDNS (local networks)

When `p2p.enable_mdns = true`, the node broadcasts its address over mDNS using the service tag `clique-consensus`. Other nodes on the same LAN respond and connections are established automatically. This is suitable for local development and testing — not for production.

### Static bootnodes

Configured via `p2p.boot_nodes` as a list of multiaddrs with peer IDs embedded:

```toml
[p2p]
boot_nodes = [
  "/ip4/1.2.3.4/tcp/9000/p2p/12D3KooWGatU9DEesagYvh8aVym6BBt7SDUDC2pwAK1zppS2HDYr",
  "/ip4/5.6.7.8/tcp/9000/p2p/12D3KooWMZVe9t3AtUp3P8vjEycXf4p9Hnq1dZx2CX1dFkWiYooh"
]
```

Boot node connections are attempted asynchronously at startup. Connection failures are logged as warnings and do not prevent the node from starting.

---

## Gossipsub Configuration

The node uses `pubsub.WithFloodPublish(true)`, which ensures messages are delivered to all directly connected peers regardless of Gossipsub mesh state. This is important for small networks (fewer than 6 peers) where the normal Gossipsub mesh does not form.

Standard Gossipsub parameters apply otherwise (D=6, D_low=4, D_high=12, heartbeat interval=1s).

---

## Connection Limits

Maximum connected peers is configured via `p2p.max_peers` (default 50). Peer pruning is handled by libp2p's connection manager when the limit is exceeded (not yet wired).

---

## Sequence: Node Startup

```
libp2p.New(ListenAddrStrings(cfg.ListenAddr))
    │
    ▼
pubsub.NewGossipSub(ctx, host, WithFloodPublish(true))
    │
    ▼
topic.Join("/clique/block/1")  →  topic.Subscribe()
    │
    ▼
host.SetStreamHandler("/clique/status/1", handleStatusStream)
    │
    ▼
host.Network().Notify(ConnectedF)    // outbound: doStatusHandshake
    │
    ▼
go subscribeBlocks(ctx)              // block topic loop
    │
    ├─ (if enable_mdns) startMDNS()
    │
    └─ for each bootnode: go host.Connect(ctx, addrInfo)
```

---

## Protocol Versioning

Both protocol IDs include a version suffix (`/1`). If the wire format changes incompatibly, the version is bumped (e.g. `/clique/block/2`). Peers running different versions will not exchange messages on that topic.
