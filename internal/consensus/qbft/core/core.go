package core

import (
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
	prepares     map[common.Address]common.Hash  // sender → blockHash
	commits      map[common.Address][]byte       // sender → commitSeal
	roundChanges map[common.Address]uint32       // sender → their round

	// backlog: messages for future rounds.
	backlog map[uint32][]IncomingMsg
}

// New creates a Core for a new block instance.
// seq is the block number. validators must be the current sorted validator list.
// quorum is the minimum number of signatures needed (2f+1).
func New(seq uint64, validators []common.Address, quorum int, cfg Config) *Core {
	vmap := make(map[common.Address]struct{}, len(validators))
	for _, v := range validators {
		vmap[v] = struct{}{}
	}
	return &Core{
		seq:          seq,
		round:        0,
		quorum:       quorum,
		validators:   vmap,
		cfg:          cfg,
		state:        stateNew,
		prepares:     make(map[common.Address]common.Hash),
		commits:      make(map[common.Address][]byte),
		roundChanges: make(map[common.Address]uint32),
		backlog:      make(map[uint32][]IncomingMsg),
	}
}

// StartProposer is called when this validator is the proposer for the current
// round. It sets up the proposal state and returns a Broadcast(PROPOSAL) decision.
func (c *Core) StartProposer(header *types.Header, payloadJSON []byte) []Decision {
	headerRLP, err := rlp.EncodeToBytes(header)
	if err != nil {
		return nil
	}
	p := Proposal{
		View:        View{Sequence: c.seq, Round: c.round},
		Header:      headerRLP,
		PayloadJSON: payloadJSON,
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

	// Also record our own PREPARE.
	// (The proposer implicitly prepares for its own proposal.)
	// We do NOT add a self-prepare here because the node is a validator too and
	// will HandleMsg its own broadcast. The self-prepare is handled symmetrically.

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
// Returns a Broadcast(ROUND_CHANGE) decision.
func (c *Core) Timeout() []Decision {
	if c.state == stateCommitted {
		return nil
	}

	rc := RoundChange{
		View:          View{Sequence: c.seq, Round: c.round},
		PreparedRound: 0,
		PreparedBlock: nil,
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
		if p.Round > c.round {
			c.backlog[p.Round] = append(c.backlog[p.Round], msg)
		}
		return nil
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
		c.state = stateCommitted

		seals := make([][]byte, 0, len(c.commits))
		for _, s := range c.commits {
			seals = append(seals, s)
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
	if rc.Round < c.round {
		return nil // stale
	}

	c.roundChanges[msg.From] = rc.Round

	// Count ROUND_CHANGE messages for the same round.
	targetRound := rc.Round
	count := 0
	for _, r := range c.roundChanges {
		if r == targetRound {
			count++
		}
	}

	if count >= c.quorum && targetRound >= c.round {
		nextRound := targetRound + 1
		return []Decision{{Type: StartRound, Round: nextRound}}
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
