// Package core implements the QBFT consensus protocol state machine.
// It is a pure state machine with no goroutines, no I/O, and no node
// dependencies. The node drives it by feeding messages and timer events;
// the core returns []Decision describing what to do next.
package core

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
)

// MsgType identifies the QBFT message kind carried in a QBFTMsg.
type MsgType uint8

const (
	MsgProposal    MsgType = 1
	MsgPrepare     MsgType = 2
	MsgCommit      MsgType = 3
	MsgRoundChange MsgType = 4
)

// View identifies the position in the consensus sequence.
type View struct {
	Sequence uint64
	Round    uint32
}

// SignedRoundChange bundles a raw ROUND_CHANGE message with the outer-envelope
// signature, for inclusion in a round-change certificate (RCC). Validators
// receiving a PROPOSAL for round > 0 verify the RCC to confirm that 2f+1
// validators agreed to advance to the new round.
type SignedRoundChange struct {
	Data []byte // RLP-encoded RoundChange
	Sig  []byte // 65-byte outer QBFTMsg signature
}

// Proposal is the QBFT PROPOSAL message payload.
type Proposal struct {
	View
	// Header is the RLP-encoded *types.Header (with proposer seal set, CommittedSeals nil).
	Header rlp.RawValue
	// PayloadJSON is the JSON-encoded engine.ExecutionPayloadV3.
	PayloadJSON []byte
	// RCC is the round-change certificate. Required when Round > 0; nil for round 0.
	// Contains 2f+1 SignedRoundChange messages proving quorum agreed to advance rounds.
	RCC []SignedRoundChange
}

// Prepare is the QBFT PREPARE message payload.
type Prepare struct {
	View
	BlockHash common.Hash
}

// Commit is the QBFT COMMIT message payload.
type Commit struct {
	View
	BlockHash  common.Hash
	CommitSeal []byte // 65-byte ECDSA seal over commitSealHash(header)
}

// RoundChange is the QBFT ROUND_CHANGE message payload.
type RoundChange struct {
	View
	// PreparedRound is the round in which the sender was last prepared.
	// Zero if the sender was not prepared before timing out.
	PreparedRound uint32
	// PreparedBlock is the RLP-encoded proposal header from PreparedRound.
	// Nil if PreparedRound == 0.
	PreparedBlock []byte
	// PreparedPayload is the JSON-encoded execution payload from PreparedRound.
	// Nil if PreparedRound == 0. Included so the new proposer can reuse the
	// exact same execution payload without rebuilding it from scratch.
	PreparedPayload []byte
}

// DecisionType classifies what action the node should take in response to a
// state transition.
type DecisionType uint8

const (
	// Broadcast instructs the node to send a QBFT message to all validators.
	Broadcast DecisionType = iota
	// CommitBlock instructs the node to finalise the block (add to store, notify EL).
	CommitBlock
	// StartRound instructs the node to advance to a new round.
	StartRound
)

// Decision is returned by the Core state machine to describe what the node
// should do. The fields populated depend on Type:
//
//   - Broadcast:   MsgType and MsgData are set; the node wraps them in a
//                  QBFTMsg (signing it) and broadcasts.
//   - CommitBlock: Header and Payload are set; the node finalises the block.
//   - StartRound:  Round, and optionally PreparedHeader/PreparedPayload/RCC,
//                  are set. The node advances to the specified round. If
//                  PreparedHeader is non-nil the new proposer must reuse it
//                  (updated round + re-sealed) rather than building fresh.
type Decision struct {
	Type    DecisionType
	MsgType MsgType
	MsgData []byte        // RLP-encoded Proposal/Prepare/Commit/RoundChange
	Header  *types.Header // set when Type == CommitBlock
	Payload []byte        // JSON payload; set when Type == CommitBlock

	// Fields set when Type == StartRound:
	Round           uint32             // the round to advance to
	PreparedHeader  *types.Header      // highest prepared header from the RCC; nil if none
	PreparedPayload []byte             // JSON payload for PreparedHeader; nil if none
	RCC             []SignedRoundChange // 2f+1 ROUND_CHANGE messages forming the certificate
}

// IncomingMsg is the input type for Core.HandleMsg. The node recovers the
// sender address from the QBFTMsg signature before dispatching.
type IncomingMsg struct {
	MsgType MsgType
	From    common.Address
	Data    []byte
	// Sig is the 65-byte outer QBFTMsg signature. Used to construct the
	// round-change certificate when the message is a ROUND_CHANGE.
	Sig []byte
}
