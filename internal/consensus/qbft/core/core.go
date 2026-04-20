package core

import (
	"bytes"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
)

// state is the QBFT instance state.
type state uint8

const (
	stateNew         state = iota // waiting for proposal
	statePrepared                 // saw valid proposal, sent PREPARE
	stateCommitSent               // saw 2f+1 PREPAREs, sent COMMIT
	stateCommitted                // saw 2f+1 COMMITs, emitted CommitBlock
	stateRoundChange              // round timer fired, waiting for 2f+1 ROUND_CHANGE
)

// roundChangeRecord stores a received ROUND_CHANGE with the metadata needed to
// build and verify a round-change certificate (RCC).
type roundChangeRecord struct {
	round           uint32
	preparedRound   uint32
	preparedBlock   []byte // RLP-encoded header; nil if not prepared
	preparedPayload []byte // JSON payload; nil if not prepared
	data            []byte // RLP-encoded RoundChange (for RCC inclusion)
	sig             []byte // outer QBFTMsg signature (for RCC inclusion)
}

// Config holds the callbacks that connect the pure state machine to the node.
type Config struct {
	// ProposalVerifier verifies a proposal header (structural + proposer authorization).
	// The node closes over the parent header and snapshot.
	ProposalVerifier func(header *types.Header) error

	// CommitSealSigner creates a commit seal for the given (sealed) proposal header.
	CommitSealSigner func(header *types.Header) ([]byte, error)

	// CommitSealVerifier recovers the signer address from a commit seal.
	CommitSealVerifier func(header *types.Header, seal []byte) (common.Address, error)

	// CommitBlock injects the collected seals into the header and returns the
	// final committed header. Corresponds to consensus.BFTEngine.CommitBlock.
	CommitBlock func(header *types.Header, seals [][]byte) (*types.Header, error)

	// RoundChangeSigVerifier recovers the validator address from the outer
	// QBFTMsg signature of a ROUND_CHANGE message. Used when verifying the
	// round-change certificate in an incoming PROPOSAL. The inputs are the
	// raw message type byte and data from the signed envelope.
	RoundChangeSigVerifier func(msgType uint8, data []byte, sig []byte) (common.Address, error)
}

// Core is the QBFT state machine for a single block instance (one sequence number
// and one round). It is not safe for concurrent use — the node must serialise
// all calls.
type Core struct {
	seq        uint64
	round      uint32
	quorum     int
	validators map[common.Address]struct{}
	cfg        Config

	state state

	// proposal received in this round.
	proposalHeader  *types.Header
	proposalPayload []byte
	proposalHash    common.Hash

	// collected messages indexed by sender.
	prepares     map[common.Address]common.Hash    // sender → blockHash
	commits      map[common.Address][]byte         // sender → commitSeal
	roundChanges map[common.Address]roundChangeRecord // sender → last ROUND_CHANGE

	// backlog: proposals for future rounds that arrived early.
	backlog map[uint32][]IncomingMsg
}

// New creates a Core for a new block instance.
// seq is the block number, round is the starting round (0 for a fresh instance,
// higher when resuming after a round change). validators must be the current
// sorted validator list. quorum is the minimum number of signatures needed (2f+1).
func New(seq uint64, round uint32, validators []common.Address, quorum int, cfg Config) *Core {
	vmap := make(map[common.Address]struct{}, len(validators))
	for _, v := range validators {
		vmap[v] = struct{}{}
	}
	return &Core{
		seq:          seq,
		round:        round,
		quorum:       quorum,
		validators:   vmap,
		cfg:          cfg,
		state:        stateNew,
		prepares:     make(map[common.Address]common.Hash),
		commits:      make(map[common.Address][]byte),
		roundChanges: make(map[common.Address]roundChangeRecord),
		backlog:      make(map[uint32][]IncomingMsg),
	}
}

// StartProposer is called when this validator is the proposer for the current
// round. It sets up the proposal state and returns Broadcast(PROPOSAL) and
// Broadcast(PREPARE) decisions.
//
// rcc must be nil for round 0. For round > 0 it must contain the 2f+1
// SignedRoundChange messages collected from the previous round's RCC.
func (c *Core) StartProposer(header *types.Header, payloadJSON []byte, rcc []SignedRoundChange) []Decision {
	headerRLP, err := rlp.EncodeToBytes(header)
	if err != nil {
		return nil
	}
	p := Proposal{
		View:        View{Sequence: c.seq, Round: c.round},
		Header:      headerRLP,
		PayloadJSON: payloadJSON,
		RCC:         rcc,
	}
	data, err := rlp.EncodeToBytes(&p)
	if err != nil {
		return nil
	}

	// Record our own proposal.
	c.proposalHeader = header
	c.proposalPayload = payloadJSON
	c.proposalHash = header.Hash()
	c.state = statePrepared

	decisions := []Decision{
		{Type: Broadcast, MsgType: MsgProposal, MsgData: data},
	}

	// After broadcasting the proposal, the proposer also sends PREPARE.
	prepare := Prepare{
		View:      View{Sequence: c.seq, Round: c.round},
		BlockHash: c.proposalHash,
	}
	prepData, err := rlp.EncodeToBytes(&prepare)
	if err != nil {
		return decisions
	}
	decisions = append(decisions, Decision{Type: Broadcast, MsgType: MsgPrepare, MsgData: prepData})
	return decisions
}

// HandleMsg processes an incoming QBFT message and returns the resulting decisions.
func (c *Core) HandleMsg(msg IncomingMsg) []Decision {
	if _, ok := c.validators[msg.From]; !ok {
		return nil // ignore messages from unknown validators
	}

	switch msg.MsgType {
	case MsgProposal:
		return c.handleProposal(msg)
	case MsgPrepare:
		return c.handlePrepare(msg)
	case MsgCommit:
		return c.handleCommit(msg)
	case MsgRoundChange:
		return c.handleRoundChange(msg)
	}
	return nil
}

// Timeout is called when the round timer fires.
// Returns a Broadcast(ROUND_CHANGE) decision. The ROUND_CHANGE View.Round is
// set to c.round+1 (the desired target round), following the standard QBFT
// convention: a ROUND_CHANGE always encodes the round the sender wants to
// advance TO, not the round it is leaving. If the sender was prepared, the
// ROUND_CHANGE includes the prepared header and payload so the new proposer
// can reuse them.
func (c *Core) Timeout() []Decision {
	if c.state == stateCommitted {
		return nil
	}

	var preparedRound uint32
	var preparedBlock, preparedPayload []byte
	if c.state == statePrepared || c.state == stateCommitSent {
		preparedRound = c.round
		if c.proposalHeader != nil {
			headerRLP, err := rlp.EncodeToBytes(c.proposalHeader)
			if err == nil {
				preparedBlock = headerRLP
			}
		}
		preparedPayload = c.proposalPayload
	}

	rc := RoundChange{
		View:            View{Sequence: c.seq, Round: c.round + 1},
		PreparedRound:   preparedRound,
		PreparedBlock:   preparedBlock,
		PreparedPayload: preparedPayload,
	}
	data, err := rlp.EncodeToBytes(&rc)
	if err != nil {
		return nil
	}

	c.state = stateRoundChange
	return []Decision{
		{Type: Broadcast, MsgType: MsgRoundChange, MsgData: data},
	}
}

// --- internal handlers ---

func (c *Core) handleProposal(msg IncomingMsg) []Decision {
	if c.state != stateNew {
		return nil // already have a proposal for this round
	}

	var p Proposal
	if err := rlp.DecodeBytes(msg.Data, &p); err != nil {
		return nil
	}
	if p.Sequence != c.seq || p.Round != c.round {
		if p.Round > c.round && len(c.backlog) < 16 {
			// Cap the backlog to bound memory usage. A Byzantine validator
			// could otherwise exhaust memory by sending proposals for many
			// different future rounds.
			c.backlog[p.Round] = append(c.backlog[p.Round], msg)
		}
		return nil
	}

	// For round > 0, verify the round-change certificate.
	if c.round > 0 {
		if !c.verifyRCC(p.RCC) {
			return nil
		}
	}

	var h types.Header
	if err := rlp.DecodeBytes(p.Header, &h); err != nil {
		return nil
	}
	if c.cfg.ProposalVerifier != nil {
		if err := c.cfg.ProposalVerifier(&h); err != nil {
			return nil
		}
	}

	c.proposalHeader = &h
	c.proposalPayload = p.PayloadJSON
	c.proposalHash = h.Hash()
	c.state = statePrepared

	// Broadcast PREPARE.
	prepare := Prepare{
		View:      View{Sequence: c.seq, Round: c.round},
		BlockHash: c.proposalHash,
	}
	data, err := rlp.EncodeToBytes(&prepare)
	if err != nil {
		return nil
	}
	return []Decision{{Type: Broadcast, MsgType: MsgPrepare, MsgData: data}}
}

// verifyRCC checks that rcc contains at least quorum valid ROUND_CHANGE
// messages for the current sequence and round. Requires cfg.RoundChangeSigVerifier.
func (c *Core) verifyRCC(rcc []SignedRoundChange) bool {
	if len(rcc) < c.quorum {
		return false
	}
	if c.cfg.RoundChangeSigVerifier == nil {
		return false // reject: cannot verify without a verifier
	}
	seen := make(map[common.Address]struct{})
	valid := 0
	for _, entry := range rcc {
		signer, err := c.cfg.RoundChangeSigVerifier(uint8(MsgRoundChange), entry.Data, entry.Sig)
		if err != nil {
			continue
		}
		if _, ok := c.validators[signer]; !ok {
			continue // not a known validator
		}
		if _, dup := seen[signer]; dup {
			continue // duplicate
		}
		var rc RoundChange
		if err := rlp.DecodeBytes(entry.Data, &rc); err != nil {
			continue
		}
		if rc.Sequence != c.seq || rc.Round != c.round {
			continue
		}
		seen[signer] = struct{}{}
		valid++
	}
	return valid >= c.quorum
}

func (c *Core) handlePrepare(msg IncomingMsg) []Decision {
	if c.state == stateNew || c.state == stateCommitted {
		return nil
	}

	var p Prepare
	if err := rlp.DecodeBytes(msg.Data, &p); err != nil {
		return nil
	}
	if p.Sequence != c.seq || p.Round != c.round {
		return nil
	}
	if p.BlockHash != c.proposalHash {
		return nil
	}

	c.prepares[msg.From] = p.BlockHash

	if c.state == statePrepared && len(c.prepares) >= c.quorum {
		c.state = stateCommitSent

		// Create and broadcast COMMIT with our seal.
		var seal []byte
		if c.cfg.CommitSealSigner != nil {
			var err error
			seal, err = c.cfg.CommitSealSigner(c.proposalHeader)
			if err != nil {
				return nil
			}
		}

		commit := Commit{
			View:       View{Sequence: c.seq, Round: c.round},
			BlockHash:  c.proposalHash,
			CommitSeal: seal,
		}
		data, err := rlp.EncodeToBytes(&commit)
		if err != nil {
			return nil
		}
		return []Decision{{Type: Broadcast, MsgType: MsgCommit, MsgData: data}}
	}
	return nil
}

func (c *Core) handleCommit(msg IncomingMsg) []Decision {
	if c.state == stateCommitted || c.state == stateNew {
		return nil
	}

	var cm Commit
	if err := rlp.DecodeBytes(msg.Data, &cm); err != nil {
		return nil
	}
	if cm.Sequence != c.seq || cm.Round != c.round {
		return nil
	}
	if cm.BlockHash != c.proposalHash {
		return nil
	}

	// Verify the commit seal.
	if c.cfg.CommitSealVerifier != nil && len(cm.CommitSeal) > 0 {
		signer, err := c.cfg.CommitSealVerifier(c.proposalHeader, cm.CommitSeal)
		if err != nil {
			return nil
		}
		if signer != msg.From {
			return nil // seal does not match claimed sender
		}
	}

	c.commits[msg.From] = cm.CommitSeal

	if len(c.commits) >= c.quorum && c.state != stateCommitted {
		// Sort by signer address for deterministic ordering so all validators
		// produce identical CommittedSeals arrays → identical header hashes.
		type addrSeal struct {
			addr common.Address
			seal []byte
		}
		items := make([]addrSeal, 0, len(c.commits))
		for addr, seal := range c.commits {
			items = append(items, addrSeal{addr, seal})
		}
		sort.Slice(items, func(i, j int) bool {
			return bytes.Compare(items[i].addr[:], items[j].addr[:]) < 0
		})
		seals := make([][]byte, len(items))
		for i, it := range items {
			seals[i] = it.seal
		}

		var finalHeader *types.Header
		if c.cfg.CommitBlock != nil {
			var err error
			finalHeader, err = c.cfg.CommitBlock(c.proposalHeader, seals)
			if err != nil {
				return nil
			}
		} else {
			finalHeader = c.proposalHeader
		}

		// Only advance to stateCommitted after CommitBlock succeeds. Setting
		// state before the callback would leave the Core permanently stuck if
		// the callback returns an error: Timeout() returns nil in stateCommitted
		// and there is no way to recover.
		c.state = stateCommitted

		return []Decision{{
			Type:    CommitBlock,
			Header:  finalHeader,
			Payload: c.proposalPayload,
		}}
	}
	return nil
}

func (c *Core) handleRoundChange(msg IncomingMsg) []Decision {
	var rc RoundChange
	if err := rlp.DecodeBytes(msg.Data, &rc); err != nil {
		return nil
	}
	if rc.Sequence != c.seq {
		return nil
	}
	if rc.Round <= c.round {
		return nil // stale: must advance beyond current round
	}

	c.roundChanges[msg.From] = roundChangeRecord{
		round:           rc.Round,
		preparedRound:   rc.PreparedRound,
		preparedBlock:   rc.PreparedBlock,
		preparedPayload: rc.PreparedPayload,
		data:            msg.Data,
		sig:             msg.Sig,
	}

	// Collect all records for the target round.
	targetRound := rc.Round
	var records []roundChangeRecord
	for _, rec := range c.roundChanges {
		if rec.round == targetRound {
			records = append(records, rec)
		}
	}

	if len(records) >= c.quorum {
		nextRound := targetRound

		// Find the record with the highest PreparedRound (the block the new
		// proposer must re-propose to preserve liveness).
		var highestPrepRound uint32
		var highestPrepBlock, highestPrepPayload []byte
		for _, rec := range records {
			if len(rec.preparedBlock) > 0 && rec.preparedRound >= highestPrepRound {
				highestPrepRound = rec.preparedRound
				highestPrepBlock = rec.preparedBlock
				highestPrepPayload = rec.preparedPayload
			}
		}

		// Build the round-change certificate. Sort by data bytes so the RCC
		// is deterministic regardless of map iteration order.
		sort.Slice(records, func(i, j int) bool {
			return bytes.Compare(records[i].data, records[j].data) < 0
		})
		rcc := make([]SignedRoundChange, len(records))
		for i, rec := range records {
			rcc[i] = SignedRoundChange{Data: rec.data, Sig: rec.sig}
		}

		// Decode the highest prepared header so the node can re-propose it.
		var preparedHeader *types.Header
		if len(highestPrepBlock) > 0 {
			var h types.Header
			if err := rlp.DecodeBytes(highestPrepBlock, &h); err == nil {
				preparedHeader = &h
			}
		}

		return []Decision{{
			Type:            StartRound,
			Round:           nextRound,
			PreparedHeader:  preparedHeader,
			PreparedPayload: highestPrepPayload,
			RCC:             rcc,
		}}
	}
	return nil
}

// BacklogForRound returns any messages that were buffered for the given round
// and clears them from the backlog. Called when the node advances to a new round.
func (c *Core) BacklogForRound(round uint32) []IncomingMsg {
	msgs := c.backlog[round]
	delete(c.backlog, round)
	return msgs
}
