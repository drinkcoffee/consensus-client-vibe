# QBFT Consensus

QBFT (Quorum Byzantine Fault Tolerance, also known as Istanbul BFT) is a three-phase Byzantine agreement protocol. Unlike Clique — where any single authorised signer can unilaterally produce a block — QBFT requires a quorum of ⌊2N/3⌋ + 1 validators to explicitly vote on each block before it is final. This gives QBFT immediate finality: once a block is committed it can never be reverted, so there is never a competing fork to resolve.

This document describes the implementation: how the protocol works, which code implements each part, and how the pieces connect.

---

## Protocol overview

Each block goes through four message types across three phases:

```
Proposer                Validator A             Validator B
   │                       │                       │
   │──── PROPOSAL ─────────▶──────────────────────▶│
   │                       │                       │
   │◀─── PREPARE ──────────│◀──────────────────────│
   │──── PREPARE ─────────▶──────────────────────▶│
   │                       │                       │
   │  (2f+1 PREPAREs seen) │                       │
   │──── COMMIT ──────────▶──────────────────────▶│
   │◀─── COMMIT ──────────│◀──────────────────────│
   │                       │                       │
   │  (2f+1 COMMITs seen — block is final)         │
```

1. **PROPOSAL** — The round's proposer builds a block, signs a proposer seal into the header, and broadcasts `PROPOSAL(header, executionPayload)` to all validators.

2. **PREPARE** — Each validator that accepts the proposal broadcasts `PREPARE(blockHash)`. This signals "I have seen a valid proposal and am ready to commit."

3. **COMMIT** — Once a validator sees ⌊2N/3⌋ + 1 PREPARE messages it broadcasts `COMMIT(blockHash, commitSeal)`, where the commit seal is an ECDSA signature over the block hash. This is the validator's binding vote.

4. **Finalise** — Once a validator sees ⌊2N/3⌋ + 1 COMMIT messages it injects the commit seals into the header's Extra field and the block is final. All validators produce an identical finalised header because the commit seals are sorted deterministically before encoding.

If the round timer fires before a block is committed, validators broadcast `ROUND_CHANGE` and the next validator in rotation becomes the proposer for the new round.

---

## Quorum

The quorum function is defined on the engine:

```go
// internal/consensus/qbft/engine.go
func (e *Engine) Quorum(validatorCount int) int {
    return (2*validatorCount)/3 + 1
}
```

For three validators: ⌊2×3/3⌋ + 1 = **3** — all three must participate. For four validators: ⌊2×4/3⌋ + 1 = **3** — three of four suffice, tolerating one Byzantine fault. The general rule is N = 3f + 1, tolerating f faults.

---

## Code layout

| Path | Contents |
|---|---|
| `internal/consensus/consensus.go` | `Engine` and `BFTEngine` interfaces |
| `internal/consensus/qbft/engine.go` | Protocol constants, header verification, sealing |
| `internal/consensus/qbft/extra.go` | `IstanbulExtra` encoding, proposer and commit seal crypto |
| `internal/consensus/qbft/snapshot.go` | Validator-set state at a block boundary |
| `internal/consensus/qbft/api.go` | Compile-time interface assertions |
| `internal/consensus/qbft/core/messages.go` | Wire types: `Proposal`, `Prepare`, `Commit`, `RoundChange`, `Decision` |
| `internal/consensus/qbft/core/core.go` | Pure QBFT state machine (`Core`) |
| `internal/node/qbft_loop.go` | Node driver: builds proposals, dispatches decisions, drives `Core` |
| `internal/p2p/host.go` | Gossipsub topic `/qbft/consensus/1`, broadcast and subscription |

---

## The `Engine` and `BFTEngine` interfaces

All consensus engines satisfy `consensus.Engine`. QBFT additionally satisfies `consensus.BFTEngine`, which the node detects with a type assertion:

```go
// internal/node/node.go
if _, isBFT := n.cliq.(consensus.BFTEngine); isBFT {
    if n.signerKey != nil {
        go n.runQBFTLoop(ctx)
    }
} else {
    n.scheduleBlockProduction(ctx)
}
```

The `BFTEngine` interface adds three methods to `Engine`:

```go
// internal/consensus/consensus.go
type BFTEngine interface {
    Quorum(validatorCount int) int
    VerifyProposal(snap Snapshot, header *types.Header, parent *types.Header) error
    CommitBlock(header *types.Header, committedSeals [][]byte) (*types.Header, error)
}
```

`VerifyProposal` is a lighter check used during the PROPOSAL phase — it validates the header structure and proposer authorisation but deliberately skips committed seal verification (those seals don't exist yet). `CommitBlock` is called once quorum COMMITs arrive: it encodes the seals into `IstanbulExtra` and returns the final header.

---

## Header Extra field — `IstanbulExtra`

QBFT stores all its consensus metadata in the block header's `Extra` field using the `IstanbulExtra` structure:

```
Extra = [32 vanity bytes | RLP(IstanbulExtra)]
```

```go
// internal/consensus/qbft/extra.go
type IstanbulExtra struct {
    Validators     []common.Address // embedded at epoch boundaries; nil otherwise
    Vote           []byte           // reserved for future validator-set voting
    Round          uint32           // QBFT round in which this block was proposed
    Seal           []byte           // 65-byte proposer ECDSA signature
    CommittedSeals [][]byte         // 65-byte commit seals from 2f+1 validators
}
```

The 32-byte vanity prefix is what gets written into the execution-layer block (Geth enforces ≤ 32 bytes on `extraData`). The full `IstanbulExtra` RLP lives only in the consensus-layer header that peers exchange over Gossipsub. This is the same CL/EL split used by Clique.

### Proposer seal

The proposer seal covers all header fields except the seal and committed seals themselves. `proposalSigHash` temporarily strips those fields from `IstanbulExtra` before hashing, so the signed data is stable:

```go
// internal/consensus/qbft/extra.go
func proposalSigHash(header *types.Header) (common.Hash, error) {
    stripped := IstanbulExtra{
        Validators:     ie.Validators,
        Vote:           ie.Vote,
        Round:          ie.Round,   // round IS part of the signed data
        Seal:           nil,        // excluded
        CommittedSeals: nil,        // excluded
    }
    // hash of RLP-encoded [ParentHash, UncleHash, ..., strippedExtra, ...]
}
```

Including `Round` in the hash means a seal from round 0 is not valid for round 1. When a proposer reuses a prepared block in a new round it must update `IstanbulExtra.Round` and re-seal — see [round-change certificates](#round-change-certificates) below.

### Commit seal

The commit seal is distinct from the proposer seal. It is computed as:

```
commitSealHash = keccak256(0x01 ++ headerHash)
```

The `0x01` prefix prevents a commit seal from being replayed as a proposer seal. The header hash used here is computed with the proposer seal intact but with `CommittedSeals` stripped:

```go
// internal/consensus/qbft/extra.go
func commitSealHash(header *types.Header) (common.Hash, error) {
    // strip CommittedSeals from IstanbulExtra, keep Seal
    // compute header.Hash() with stripped extra
    return crypto.Keccak256Hash(append([]byte{0x01}, h.Bytes()...)), nil
}
```

### Deterministic seal ordering

All validators independently collect commit seals from quorum COMMIT messages and inject them into the header. For all validators to produce the same header hash, the seals must be sorted in the same order. The core sorts by signer address before building the slice:

```go
// internal/consensus/qbft/core/core.go
sort.Slice(items, func(i, j int) bool {
    return items[i].addr.Hex() < items[j].addr.Hex()
})
```

---

## The state machine — `Core`

`Core` (`internal/consensus/qbft/core/core.go`) is a pure state machine: no goroutines, no I/O, no node dependencies. The node feeds it events; it returns a slice of `Decision` values describing what to do next.

### States

```go
stateNew         // waiting for PROPOSAL
statePrepared    // received valid PROPOSAL, sent PREPARE
stateCommitSent  // saw 2f+1 PREPAREs, sent COMMIT
stateCommitted   // saw 2f+1 COMMITs, emitted CommitBlock decision
stateRoundChange // timer fired, sent ROUND_CHANGE
```

### API

```go
// Create a Core for block number seq, with the given sorted validator list and quorum.
func New(seq uint64, validators []common.Address, quorum int, cfg Config) *Core

// Called when this node is the proposer. Returns Broadcast(PROPOSAL) and Broadcast(PREPARE).
func (c *Core) StartProposer(header *types.Header, payloadJSON []byte, rcc []SignedRoundChange) []Decision

// Feed an incoming message. Returns zero or more decisions.
func (c *Core) HandleMsg(msg IncomingMsg) []Decision

// Feed a timer expiry. Returns Broadcast(ROUND_CHANGE).
func (c *Core) Timeout() []Decision

// Return and clear proposals buffered for a future round.
func (c *Core) BacklogForRound(round uint32) []IncomingMsg
```

A separate `Core` instance is created for each (sequence, round) pair. When the round advances the node creates a fresh `Core` for the new round.

### Decision types

```go
// internal/consensus/qbft/core/messages.go
const (
    Broadcast   // send a QBFT message to all peers
    CommitBlock // finalise the block: inject seals, import to EL, update store
    StartRound  // advance to a new round
)
```

`CommitBlock` carries the final header (with committed seals) and the serialised execution payload. `StartRound` carries the round number to jump to, and optionally the highest prepared block from the round-change certificate — see below.

### Config callbacks

The core communicates outward only via callbacks in `Config`. This is what makes it a pure state machine:

```go
// internal/consensus/qbft/core/core.go
type Config struct {
    ProposalVerifier       func(header *types.Header) error
    CommitSealSigner       func(header *types.Header) ([]byte, error)
    CommitSealVerifier     func(header *types.Header, seal []byte) (common.Address, error)
    CommitBlock            func(header *types.Header, seals [][]byte) (*types.Header, error)
    RoundChangeSigVerifier func(msgType uint8, data []byte, sig []byte) (common.Address, error)
}
```

The node closes over the current snapshot and parent header when wiring these up in `runQBFTInstance`.

---

## Message signing and sender authentication

QBFT messages are sent over Gossipsub as `QBFTMsg` values:

```go
// internal/p2p/types.go
type QBFTMsg struct {
    Type uint8  // MsgProposal=1, MsgPrepare=2, MsgCommit=3, MsgRoundChange=4
    Data []byte // RLP-encoded Proposal/Prepare/Commit/RoundChange
    Sig  []byte // 65-byte ECDSA signature — authenticates the sender
}
```

The outer signature covers `keccak256(RLP([Type, Data]))`:

```go
// internal/node/qbft_loop.go
func qbftMsgHash(m *p2phost.QBFTMsg) common.Hash {
    enc, _ := rlp.EncodeToBytes([]interface{}{m.Type, m.Data})
    return gethcrypto.Keccak256Hash(enc)
}
```

Before passing a message to `core.HandleMsg`, the node recovers the sender address from the signature:

```go
// internal/node/qbft_loop.go — inside qbftRoundLoop
from, err := recoverQBFTMsgSender(rawMsg)
incoming := qbftcore.IncomingMsg{
    MsgType: qbftcore.MsgType(rawMsg.Type),
    From:    from,
    Data:    rawMsg.Data,
    Sig:     rawMsg.Sig,  // kept for round-change certificate construction
}
decisions := core.HandleMsg(incoming)
```

The core ignores messages from addresses not in the current validator set:

```go
// internal/consensus/qbft/core/core.go
func (c *Core) HandleMsg(msg IncomingMsg) []Decision {
    if _, ok := c.validators[msg.From]; !ok {
        return nil
    }
    // ...
}
```

The `Sig` field is stored alongside the `RoundChange` data so it can be included verbatim in a round-change certificate.

---

## The node driver — `runQBFTInstance`

`runQBFTInstance` in `internal/node/qbft_loop.go` is responsible for one block number. It loops over rounds until the block is committed.

### Round loop structure

```
for round := 0; ; {
    isProposer = validators[(blockNum + round) % N] == signerAddr

    core = Core.New(seq, validators, quorum, cfg)
    replay backlog from previous core for this round

    if isProposer:
        if round > 0 and RCC has a prepared block:
            header = reuseQBFTPreparedHeader(preparedHeader, round)
            payload = preparedPayload
        else:
            header, payload = buildQBFTProposal(...)   // FCU + GetPayload
        decisions = core.StartProposer(header, payload, rcc)
        dispatch decisions

    qbftRoundLoop(core, timer):
        select:
            msg from qbftMsgCh → core.HandleMsg(msg) → dispatch decisions
            timer fires        → core.Timeout()      → dispatch decisions
            ctx cancelled      → return

    if CommitBlock:  return finalHeader, finalPayload
    if StartRound:   round = decision.Round; continue
}
```

### Proposer rotation

The round-0 proposer for block N is `validators[N % len(validators)]`. For each subsequent round the index advances by one: `validators[(N + round) % len(validators)]`. This ensures a different validator gets a chance to propose if the current one fails:

```go
// internal/node/qbft_loop.go
func (n *Node) isQBFTProposerForRound(snap consensus.Snapshot, number uint64, round uint32) bool {
    validators := snap.SignerList()
    idx := (int(number) + int(round)) % len(validators)
    return validators[idx] == n.signerAddr
}
```

### Building a proposal

The proposer calls `buildQBFTProposal`, which:
1. Waits until `parent.Time + period` so the block timestamp is valid.
2. Calls `engine_forkchoiceUpdatedV3` with `payloadAttributes` to start EL block building.
3. Calls `engine_getPayloadV3` to fetch the completed execution payload.
4. Assembles the CL header from the payload fields (`Root`, `ReceiptHash`, `GasUsed`, etc.).
5. Calls `SealHeader` to write the 65-byte proposer ECDSA signature into `IstanbulExtra.Seal`.

### Dispatching decisions

`dispatchQBFTDecisions` in `qbft_loop.go` executes what the core asks for:

- **Broadcast**: signs the `QBFTMsg` with the node's validator key, publishes it on the `/qbft/consensus/1` Gossipsub topic, then echoes it back into `qbftMsgCh` so the local core counts the node's own message.
- **CommitBlock**: unmarshals the execution payload, calls `engine_newPayloadV3` to deliver it to the EL, adds the final header to the fork-choice store, calls `engine_forkchoiceUpdatedV3` to update the EL's head, and broadcasts the committed block on the `/consensus/block/1` topic for followers.
- **StartRound**: returns the `NextRound`, `PreparedHeader`, `PreparedPayload`, and `RCC` to the caller so the round loop can advance.

---

## Round-change certificates

When the round timer fires, `Core.Timeout()` broadcasts a `ROUND_CHANGE` message:

```go
// internal/consensus/qbft/core/core.go
func (c *Core) Timeout() []Decision {
    var preparedRound uint32
    var preparedBlock, preparedPayload []byte
    if c.state == statePrepared || c.state == stateCommitSent {
        // This node saw a valid PROPOSAL and sent PREPARE or COMMIT before
        // the timer fired. Include the block so the next proposer can reuse it.
        preparedRound = c.round
        preparedBlock  = rlp.EncodeToBytes(c.proposalHeader)
        preparedPayload = c.proposalPayload
    }
    rc := RoundChange{
        View:            View{Sequence: c.seq, Round: c.round},
        PreparedRound:   preparedRound,
        PreparedBlock:   preparedBlock,   // RLP-encoded header
        PreparedPayload: preparedPayload, // JSON execution payload
    }
    // ...
}
```

When `handleRoundChange` accumulates quorum ROUND_CHANGE messages for the same round it:

1. Finds the record with the highest `PreparedRound` across all collected messages.
2. Assembles a **round-change certificate** (RCC): the 2f+1 signed `{Data, Sig}` pairs.
3. Returns a `StartRound` decision carrying the `NextRound`, the decoded `PreparedHeader`, the `PreparedPayload`, and the `RCC`.

The next proposer must reuse the highest prepared block if one exists. It calls `reuseQBFTPreparedHeader` to update the round number inside `IstanbulExtra` and re-seal with its own key:

```go
// internal/node/qbft_loop.go
func (n *Node) reuseQBFTPreparedHeader(prepared *types.Header, newRound uint32) (*types.Header, error) {
    ie, _ := qbfteng.DecodeExtra(prepared)
    ie.Round = newRound
    ie.Seal = nil
    ie.CommittedSeals = nil
    extra, _ := qbfteng.EncodeExtra(prepared.Extra[:qbfteng.ExtraVanity], ie)
    h := *prepared
    h.Extra = extra
    qeng.SealHeader(&h, n.signerKey)
    return &h, nil
}
```

The `PROPOSAL` for round > 0 includes the RCC, and `handleProposal` calls `verifyRCC` to check it before accepting the proposal. `verifyRCC` recovers each signer from the outer `QBFTMsg.Sig`, checks it is a known validator, decodes the `RoundChange` payload and verifies the sequence and round match, and counts distinct valid entries against the quorum:

```go
// internal/consensus/qbft/core/core.go
func (c *Core) verifyRCC(rcc []SignedRoundChange) bool {
    // ...
    for _, entry := range rcc {
        signer, err := c.cfg.RoundChangeSigVerifier(uint8(MsgRoundChange), entry.Data, entry.Sig)
        // check signer is a validator, no duplicates, correct sequence/round
        valid++
    }
    return valid >= c.quorum
}
```

**Why this matters for liveness.** Suppose a round-0 proposer broadcasts a valid block and f+1 honest validators reach `statePrepared`. If the timer fires before those validators send 2f+1 COMMITs, they include the prepared block in their ROUND_CHANGE. The round-1 proposer extracts it from the RCC and re-proposes the same execution payload, ensuring the honest validators can finish committing that block rather than discarding it and starting over. Without round-change certificates the protocol could stall, requiring additional full rounds to commit a block that was already almost done.

---

## Backlog replay

A PROPOSAL for round 1 might arrive while the node is still processing round 0. `handleProposal` saves out-of-turn proposals to the backlog:

```go
// internal/consensus/qbft/core/core.go — handleProposal
if p.Round > c.round {
    c.backlog[p.Round] = append(c.backlog[p.Round], msg)
}
```

When the round advances, the node calls `BacklogForRound` on the old core and feeds the stored messages directly into the new core before entering the event loop:

```go
// internal/node/qbft_loop.go
if prevCore != nil {
    for _, backlogged := range prevCore.BacklogForRound(round) {
        _ = core.HandleMsg(backlogged)
    }
}
```

This means a proposer's PROPOSAL that arrived early is not lost when the node advances to the round it belongs to.

---

## Validator set and snapshots

The validator set is managed by `qbft.Snapshot`:

```go
// internal/consensus/qbft/snapshot.go
type Snapshot struct {
    Number     uint64
    Hash       common.Hash
    Validators map[common.Address]struct{}
}
```

The initial validator set is read from the genesis block's `Extra` field. QBFT supports both native `IstanbulExtra` format and the Clique-format genesis (`[32 vanity][N×20 addrs][65 seal]`), so a standard Geth `geth init` genesis works without modification:

```go
// internal/consensus/qbft/engine.go — NewGenesisSnapshot
// Try native QBFT IstanbulExtra format first.
if ie, err := DecodeExtra(genesis); err == nil && len(ie.Validators) > 0 {
    return newSnapshot(0, genesis.Hash(), ie.Validators), nil
}
// Fall back to Clique-format genesis.
middle := extra[ExtraVanity : len(extra)-ExtraSeal]
validators := make([]common.Address, len(middle)/common.AddressLength)
```

At epoch boundaries the validator set embedded in `IstanbulExtra.Validators` replaces the previous set. Between epoch boundaries the validator set is unchanged:

```go
// internal/consensus/qbft/snapshot.go — apply
for _, h := range headers {
    if num%epoch == 0 {
        // Read new validator set from the checkpoint header.
        ie, _ := DecodeExtra(h)
        next.Validators = makeValidatorMap(ie.Validators)
    }
}
```

`SignerList()` always returns validators sorted lexicographically by address. This ordering is the canonical order used everywhere — proposer rotation, committed seal sorting, and epoch checkpoint encoding all use the same sorted list.

---

## Follower mode

A node without a configured `validator_key_path` runs in follower mode. It does not participate in the QBFT protocol loop:

```go
// internal/node/node.go — Start
if _, isBFT := n.cliq.(consensus.BFTEngine); isBFT {
    if n.signerKey != nil {
        go n.runQBFTLoop(ctx)
    }
    // followers do nothing here
}
```

Followers receive committed blocks via the `/consensus/block/1` Gossipsub topic through the standard `handleBlock` path. Each committed block arrives with the full execution payload encoded alongside the CL header. `handleBlock` calls `VerifyHeader` (which checks committed seals satisfy quorum and all signers are authorised validators), imports the payload to the local EL, and updates fork choice. Followers track the canonical chain passively and do not need to know about rounds or PREPARE/COMMIT messages.

---

## Data flow summary

```
Proposer                               Validators                       Follower
───────────────────────────────────────────────────────────────────────────────
FCU+GetPayload (EL)
SealHeader
StartProposer → Broadcast(PROPOSAL)
                                ──── Gossipsub /qbft/consensus/1 ────▶
                                       HandleMsg(PROPOSAL)
                                       Broadcast(PREPARE)
                                ◀─────────────────────────────────────
HandleMsg(PREPARE) ×(2f+1)
Broadcast(COMMIT)
                                ──── Gossipsub /qbft/consensus/1 ────▶
                                       HandleMsg(COMMIT)×(2f+1)
                                       CommitBlock decision:
                                         engine_newPayload (EL)
                                         store.AddBlock
                                         engine_forkchoiceUpdated (EL)
                                         Broadcast committed block
                                                        ─── /consensus/block/1 ──▶
                                                               VerifyHeader
                                                               engine_newPayload
                                                               engine_forkchoiceUpdated
```

The committed block broadcast on `/consensus/block/1` carries the full execution payload, so the follower's execution client receives it via `engine_newPayloadV3` without waiting for devp2p peer sync.
