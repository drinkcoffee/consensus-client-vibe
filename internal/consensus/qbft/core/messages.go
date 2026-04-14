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

// Proposal is the QBFT PROPOSAL message payload.
type Proposal struct {
	View
	// Header is the RLP-encoded *types.Header (with proposer seal set, CommittedSeals nil).
	Header      rlp.RawValue
	// PayloadJSON is the JSON-encoded engine.ExecutionPayloadV3.
	PayloadJSON []byte
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
	PreparedRound uint32
	PreparedBlock []byte // RLP-encoded header if prepared; nil otherwise
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
//   - StartRound:  Round is set; the node creates a new Core instance for that
//                  round (reusing the same sequence number).
type Decision struct {
	Type    DecisionType
	MsgType MsgType
	MsgData []byte         // RLP-encoded Proposal/Prepare/Commit/RoundChange
	Header  *types.Header  // set when Type == CommitBlock
	Payload []byte         // JSON payload; set when Type == CommitBlock
	Round   uint32         // set when Type == StartRound
}

// IncomingMsg is the input type for Core.HandleMsg. The node recovers the
// sender address from the QBFTMsg signature before dispatching.
type IncomingMsg struct {
	MsgType MsgType
	From    common.Address
	Data    []byte // RLP-encoded Proposal/Prepare/Commit/RoundChange
}
