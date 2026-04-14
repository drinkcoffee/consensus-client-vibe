# Plan: Adding QBFT Consensus

## Background: why QBFT is architecturally different from Clique

Clique is leaderless-gossip: each signer independently seals a block and broadcasts it;
peers accept or reject. QBFT is a three-phase Byzantine agreement protocol:

1. **PROPOSAL** — the round's proposer builds a block, adds a proposer seal, and broadcasts
   it to all validators
2. **PREPARE** — each validator that accepts the proposal broadcasts `PREPARE(blockHash)`
3. **COMMIT** — once a validator sees 2f+1 PREPAREs it broadcasts
   `COMMIT(blockHash, commitSeal)`, where the commit seal is an ECDSA signature over the
   block hash and round number
4. **Finalise** — once a validator sees 2f+1 COMMITs it injects the commit seals into the
   block's Extra field and broadcasts/imports the final block

If the round times out without a commit, validators broadcast `ROUND_CHANGE` and the next
validator in rotation becomes proposer. QBFT therefore requires a real-time multi-party
message exchange that Clique does not.

---

## What can be reused unchanged

| Component | Notes |
|---|---|
| `consensus.Engine` / `consensus.Snapshot` interfaces | Sound abstractions; need modest additions, not replacement |
| Fork-choice store | Header-only, consensus-agnostic; works for QBFT (blocks are always final so there is never a competing fork) |
| Engine API interactions (FCU, GetPayload, NewPayload) | Identical for both protocols |
| Sync protocol and sync messages | `SyncBlock` is just header + payload + ELHash |
| P2P host infrastructure | libp2p, gossipsub, stream handlers all reusable |
| `handleBlock` gossip path | Final QBFT blocks (with committed seals) arrive via the existing gossip topic |

---

## Step 1 — Interface additions

**`internal/consensus/consensus.go`**

Add one method to `Engine`:

```go
// BuildExtra returns the CL-side Extra bytes for a new block at number.
// The CL extra differs from what is sent to the EL (which always receives
// only a 32-byte vanity field). Each consensus engine encodes its metadata
// (signer list, committed seals, etc.) in its own format.
BuildExtra(snap Snapshot, number uint64) []byte
```

This moves the `buildExtra` logic out of `node/block_producer.go` and into the engine,
which is the right place for a format that is consensus-specific. Clique's implementation
is exactly the existing `buildExtra` body. QBFT's implementation encodes `IstanbulExtra`
instead.

Add a new optional interface:

```go
// BFTEngine is an optional extension of Engine for protocols that require
// multi-validator agreement before a block is final. The node detects it via
// type assertion and replaces the timer-based production loop with a BFT
// message loop.
type BFTEngine interface {
    // Quorum is the minimum number of validator signatures to commit a block.
    // For QBFT: floor(2N/3) + 1
    Quorum(validatorCount int) int

    // VerifyProposal validates a block received in the PROPOSAL phase.
    // Unlike VerifyHeader it does not check committed seals (they do not exist
    // yet). Called by the QBFT core when processing an incoming PROPOSAL.
    VerifyProposal(snap Snapshot, header *types.Header, parent *types.Header) error

    // CommitBlock injects the collected committed seals into header's Extra
    // field and returns the new final header. Called once 2f+1 COMMIT messages
    // have been collected.
    CommitBlock(header *types.Header, committedSeals [][]byte) (*types.Header, error)
}
```

`consensus.Snapshot` needs no changes. All fields needed by QBFT (`SignerList`,
`IsAuthorized`, `InTurn`) are already there. `HasRecentlySigned` and `PendingVotes`
become no-ops in the QBFT implementation.

---

## Step 2 — `internal/consensus/qbft/` package

Four files, mirroring the clique package structure.

### `engine.go`

```go
type Engine struct {
    period         uint64
    epoch          uint64
    requestTimeout time.Duration
}

func New(period, epoch uint64, timeout time.Duration) *Engine
```

Notable differences from Clique:

- `CalcDifficulty` always returns `big.NewInt(1)`. Difficulty is meaningless in QBFT;
  blocks are immediately final so there is never a competing fork.
- `NonceAuth` / `NonceDrop` return a zero nonce. QBFT does not use nonce-based on-chain
  voting.
- `VerifyHeader` checks that the committed seals in `IstanbulExtra` satisfy quorum, in
  addition to the structural checks Clique performs.
- `SealHeader` signs the block hash with the proposer's key and writes the 65-byte
  proposer seal into `Extra[ExtraVanity : ExtraVanity+65]`.
- `SignerFromHeader` recovers the proposer address from that same seal.
- `BuildExtra` encodes `IstanbulExtra{Validators: [...] at epoch boundaries, Round: 0,
  CommittedSeals: nil}` as `vanity | RLP(IstanbulExtra)`.

Implements both `consensus.Engine` and `consensus.BFTEngine`.

### `snapshot.go`

```go
type Snapshot struct {
    Number     uint64
    Hash       common.Hash
    Validators map[common.Address]struct{}
}
```

- `SignerList()` returns validators sorted by address (same as Clique).
- `InTurn(number, signer)` — round-0 proposer is `validators[number % N]`. Also used by
  `scheduleBlockProduction` to determine the delay before proposing.
- `HasRecentlySigned` — always returns `false`. QBFT does not enforce a per-validator
  cooldown at the snapshot level; the BFT protocol prevents double proposals.
- `PendingVotes` — returns `nil`. Validator set changes are handled at epoch boundaries,
  not via accumulated votes.
- `Apply(headers)` — at epoch boundaries reads the new validator list from
  `IstanbulExtra.Validators`; otherwise no change.

### `extra.go`

```go
type IstanbulExtra struct {
    Validators     []common.Address
    Vote           []byte   // optional RLP(ValidatorVote); nil if no vote
    Round          uint32
    CommittedSeals [][]byte // 65-byte ECDSA seals; nil in PROPOSAL headers
}

func EncodeExtra(vanity []byte, ie *IstanbulExtra) ([]byte, error)
func DecodeExtra(header *types.Header) (*IstanbulExtra, error)
```

The Extra layout is `[32 vanity bytes | RLP(IstanbulExtra)]`. The EL always receives only
the 32-byte vanity portion — the same CL/EL separation as Clique today.

### `api.go`

Compile-time interface assertions:

```go
var _ consensus.Engine    = (*Engine)(nil)
var _ consensus.Snapshot  = (*Snapshot)(nil)
var _ consensus.BFTEngine = (*Engine)(nil)
```

---

## Step 3 — `internal/consensus/qbft/core/` — the state machine

The core package implements the QBFT protocol for a single block instance. It is a **pure
state machine with no goroutines, no I/O, and no node dependencies**. The node drives it
by feeding messages and timer events in; the core returns `[]Decision` describing what to
do next.

```go
type DecisionType uint8
const (
    Broadcast    DecisionType = iota // send Msg to all validators via P2P
    CommitBlock                      // import Header+Payload to EL+store; advance snapshot
    StartRound                       // advance round: reset timer, start proposer logic
)

type Decision struct {
    Type    DecisionType
    Msg     *QBFTMsg       // set when Type == Broadcast
    Header  *types.Header  // set when Type == CommitBlock
    Payload []byte         // JSON payload; set when Type == CommitBlock
    Round   uint32         // set when Type == StartRound
}
```

### Core API

```go
type Core struct { /* round, state, collected messages, quorum, validators */ }

func New(seq uint64, validators []common.Address, quorum int, timeout time.Duration) *Core

// StartProposer is called when this validator is the proposer for the current
// round. Returns a Broadcast(PROPOSAL) decision immediately.
func (c *Core) StartProposer(header *types.Header, payloadJSON []byte) []Decision

// HandleMsg processes an incoming QBFT message and returns the resulting decisions.
// Called by the node for every message arriving on /qbft/consensus/1.
func (c *Core) HandleMsg(msg *QBFTMsg) []Decision

// Timeout is called when the round timer fires.
// Returns a Broadcast(ROUND_CHANGE) and a StartRound decision.
func (c *Core) Timeout() []Decision
```

### State transitions

```
PRE_PREPARE
  <- StartProposer / receive valid PROPOSAL
  -> emit Broadcast(PREPARE)
  -> state = PREPARED

PREPARED
  <- receive 2f+1 PREPARE messages
  -> emit Broadcast(COMMIT)
  -> state = COMMIT_SENT

COMMIT_SENT
  <- receive 2f+1 COMMIT messages
  -> emit CommitBlock(finalHeader)
  -> state = COMMITTED

COMMITTED (terminal for this instance)

ROUND_CHANGE (on timeout while not COMMITTED)
  -> emit Broadcast(ROUND_CHANGE)
  <- receive 2f+1 ROUND_CHANGE for same round
  -> emit StartRound(round+1)
  -> node creates new Core instance for round+1
```

### Message types (`messages.go`)

```go
type View struct { Sequence uint64; Round uint32 }

type Proposal    struct { View; Header []byte; PayloadJSON []byte; ProposerSeal []byte }
type Prepare     struct { View; BlockHash common.Hash; Sig []byte }
type Commit      struct { View; BlockHash common.Hash; CommitSeal []byte }
type RoundChange struct { View; PreparedRound uint32; PreparedBlock []byte }

// QBFTMsg is the wire envelope for all four message types.
type QBFTMsg struct {
    Type uint8
    Data []byte // RLP-encoded Proposal/Prepare/Commit/RoundChange
    Sig  []byte // ECDSA signature over keccak256(Type ++ Data) — authenticates sender
}
```

A backlog (map from round to unseen messages) handles messages arriving out-of-order from
future rounds.

---

## Step 4 — P2P layer additions

**Rename block gossip topic** from `/clique/block/1` to `/consensus/block/1`. Now is the
right time; otherwise this string becomes permanently misleading. It is a one-line change
in `host.go`.

**New QBFT consensus message topic** added to `internal/p2p/host.go`:

```go
const qbftTopic = "/qbft/consensus/1"

// New fields on Host:
qt   *pubsub.Topic
qsub *pubsub.Subscription

// New exported API:
func (h *Host) SetQBFTMsgHandler(fn func(*QBFTMsg))
func (h *Host) BroadcastQBFTMsg(ctx context.Context, msg *QBFTMsg) error
```

`Start()` joins the QBFT topic and starts a subscription loop analogous to
`subscribeBlocks`. `QBFTMsg` is added to `types.go` and is RLP-encoded on the wire like
`CliqueBlock`.

---

## Step 5 — Node integration

### `node.go` — constructor and `Start()`

In `New()`, the consensus factory gains the `qbft` case:

```go
case "qbft":
    cliq = qbfteng.New(
        cfg.Consensus.QBFT.Period,
        cfg.Consensus.QBFT.Epoch,
        time.Duration(cfg.Consensus.QBFT.RequestTimeoutMs)*time.Millisecond,
    )
```

In `Start()`, after registering block/sync handlers:

```go
n.p2p.SetQBFTMsgHandler(n.handleQBFTMsg)

if _, isBFT := n.cliq.(consensus.BFTEngine); isBFT {
    go n.runQBFTLoop(ctx)
} else {
    n.scheduleBlockProduction(ctx)
}
```

Add to the `Node` struct:

```go
qbftMsgCh chan *p2phost.QBFTMsg // P2P handler -> active QBFT instance
```

### New file `internal/node/qbft_loop.go`

```go
// runQBFTLoop runs the QBFT consensus protocol indefinitely. It starts a new
// instance for each block slot and blocks until that block is committed or
// the context is cancelled.
func (n *Node) runQBFTLoop(ctx context.Context)

// runQBFTInstance handles one block slot. Drives the QBFT core state machine:
// feeds it messages from qbftMsgCh and fires the round timer.
// Returns the committed header and payload on success.
func (n *Node) runQBFTInstance(
    ctx context.Context,
    parent *types.Header,
    snap consensus.Snapshot,
) (*types.Header, engine.ExecutionPayloadV3, error)

// handleQBFTMsg is the p2p.QBFTMsgHandler. Forwards incoming messages to
// the active instance via qbftMsgCh (non-blocking; drops if no instance running).
func (n *Node) handleQBFTMsg(msg *p2phost.QBFTMsg)
```

`runQBFTInstance` is responsible for:

1. Determining if this node is proposer (`snap.InTurn(nextNum, signerAddr)` for round 0)
2. If proposer: calling FCU+GetPayload to build the block, then `core.StartProposer`
3. Entering a select loop over `qbftMsgCh` and a round timer, feeding events to the core
4. When the core returns a `CommitBlock` decision: calling `eng.NewPayloadV3`,
   `stor.AddBlock`, `eng.ForkchoiceUpdatedV3`, updating snapshots and P2P status

`runQBFTLoop` calls `runQBFTInstance` in a loop, advancing `parent` and `snap` after each
committed block.

### `block_producer.go`

`buildExtra` is deleted; callers are replaced with `n.cliq.BuildExtra(snap, nextNum)`.

---

## Step 6 — Config

`internal/config/config.go`:

```go
type ConsensusConfig struct {
    Type   string       `toml:"type"`
    Clique CliqueConfig `toml:"clique"`
    QBFT   QBFTConfig   `toml:"qbft"`
}

type QBFTConfig struct {
    ValidatorKeyPath string `toml:"validator_key_path"`
    Period           uint64 `toml:"period"`
    Epoch            uint64 `toml:"epoch"`
    RequestTimeoutMs uint64 `toml:"request_timeout_ms"`
}
```

`config.example.toml` gains a commented-out `[consensus.qbft]` section.

---

## Files changed / created

| File | Change |
|---|---|
| `internal/consensus/consensus.go` | Add `BuildExtra` to `Engine`; add `BFTEngine` optional interface |
| `internal/consensus/clique/engine.go` | Add `BuildExtra` method (move body from node layer) |
| `internal/consensus/clique/api.go` | Add `BuildExtra` to compile-time check |
| `internal/consensus/qbft/engine.go` | New |
| `internal/consensus/qbft/snapshot.go` | New |
| `internal/consensus/qbft/extra.go` | New |
| `internal/consensus/qbft/api.go` | New |
| `internal/consensus/qbft/core/messages.go` | New |
| `internal/consensus/qbft/core/core.go` | New |
| `internal/p2p/host.go` | Add QBFT topic, `SetQBFTMsgHandler`, `BroadcastQBFTMsg`; rename block topic |
| `internal/p2p/types.go` | Add `QBFTMsg` |
| `internal/node/node.go` | Detect `BFTEngine`; add `qbftMsgCh`; QBFT factory case |
| `internal/node/block_producer.go` | Replace `buildExtra` call with `n.cliq.BuildExtra` |
| `internal/node/qbft_loop.go` | New |
| `internal/config/config.go` | Add `QBFTConfig` |

---

## Key risks and open questions

**Round-change certificates.** The full QBFT spec requires a `PREPARED` certificate (the
2f+1 PREPARE messages from the last prepared round) to be included in `ROUND_CHANGE`
messages; the new proposer must use the highest-round prepared block. This is the most
complex part of the spec. A minimal first implementation can omit this and simply start
from an empty proposal in every new round — correct but potentially liveness-reducing
under certain failure patterns.

**Validator key path.** Clique and QBFT both use an ECDSA key loaded from a file. Whether
to unify `validator_key_path` and `signer_key_path` into a single config key is a naming
decision with no functional consequence.

**Validator set changes.** The plan above uses genesis-defined and epoch-checkpoint
validator sets only. Adding runtime validator management (contract-based or vote-based) is
a separate future item.

**`handleBlock` and QBFT blocks.** The existing `handleBlock` gossip path calls
`n.cliq.VerifyHeader`, which for QBFT validates the committed seals. This correctly
handles both the case where a non-validator node receives a final QBFT block (passive sync
path) and the case where a validator that already committed the block receives a duplicate
gossip (`stor.AddBlock` returns false for duplicates, so the second path is a no-op).
