package node

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	gethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"

	"github.com/peterrobinson/consensus-client-vibe/internal/consensus"
	qbfteng "github.com/peterrobinson/consensus-client-vibe/internal/consensus/qbft"
	qbftcore "github.com/peterrobinson/consensus-client-vibe/internal/consensus/qbft/core"
	"github.com/peterrobinson/consensus-client-vibe/internal/engine"
	p2phost "github.com/peterrobinson/consensus-client-vibe/internal/p2p"
)

// handleQBFTMsg is the p2p.QBFTMsgHandler. It forwards incoming messages to
// the active QBFT instance via qbftMsgCh (non-blocking; drops if no instance
// is running or the channel is full).
func (n *Node) handleQBFTMsg(msg *p2phost.QBFTMsg) {
	select {
	case n.qbftMsgCh <- msg:
	default:
		n.log.Debug().Msg("qbft: dropped incoming message (channel full or no active instance)")
	}
}

// runQBFTLoop runs the QBFT consensus protocol indefinitely. It starts a new
// instance for each block slot and blocks until that block is committed or the
// context is cancelled. Only called when the engine implements consensus.BFTEngine.
func (n *Node) runQBFTLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		parent := n.stor.Head()
		if parent == nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		snap := n.headSnapshot()
		if snap == nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		committedHeader, committedPayload, err := n.runQBFTInstance(ctx, parent, snap)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			n.log.Error().Err(err).Msg("qbft: instance failed")
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if committedHeader == nil {
			return // context cancelled
		}

		newSnap, err := n.cliq.Apply(snap, []*types.Header{committedHeader})
		if err != nil {
			n.log.Error().Err(err).Msg("qbft: apply snapshot failed")
		} else {
			n.setHeadSnapshot(newSnap)
		}

		if n.p2p != nil {
			n.p2p.SetStatus(p2phost.StatusMsg{
				NetworkID:   n.cfg.Node.NetworkID,
				GenesisHash: n.genesisSnap.BlockHash(),
				HeadHash:    committedHeader.Hash(),
				HeadNumber:  committedHeader.Number.Uint64(),
			})
		}

		n.log.Info().
			Uint64("number", committedHeader.Number.Uint64()).
			Str("hash", committedHeader.Hash().Hex()).
			Msg("qbft: block committed")

		_ = committedPayload
	}
}

// runQBFTInstance drives the QBFT state machine for a single block number.
// It loops over rounds until either the block is committed or ctx is cancelled.
// Returns (nil, nil, nil) if ctx was cancelled.
func (n *Node) runQBFTInstance(
	ctx context.Context,
	parent *types.Header,
	snap consensus.Snapshot,
) (*types.Header, *engine.ExecutionPayloadV3, error) {
	bftEng, ok := n.cliq.(consensus.BFTEngine)
	if !ok {
		return nil, nil, fmt.Errorf("qbft: engine does not implement BFTEngine")
	}
	qeng, ok := n.cliq.(*qbfteng.Engine)
	if !ok {
		return nil, nil, fmt.Errorf("qbft: engine is not *qbft.Engine")
	}

	nextNum := parent.Number.Uint64() + 1
	validators := snap.SignerList()
	quorum := bftEng.Quorum(len(validators))
	timeout := qeng.RequestTimeout()
	if timeout == 0 {
		timeout = 4 * time.Second
	}

	for round := uint32(0); ; round++ {
		select {
		case <-ctx.Done():
			return nil, nil, nil
		default:
		}

		isProposer := n.signerKey != nil && n.isQBFTProposerForRound(snap, nextNum, round)

		core := qbftcore.New(nextNum, validators, quorum, qbftcore.Config{
			ProposalVerifier: func(h *types.Header) error {
				return bftEng.VerifyProposal(snap, h, parent)
			},
			CommitSealSigner: func(h *types.Header) ([]byte, error) {
				if n.signerKey == nil {
					return nil, nil
				}
				return qeng.CreateCommitSeal(h, n.signerKey)
			},
			CommitSealVerifier: func(h *types.Header, seal []byte) (common.Address, error) {
				return qeng.RecoverCommitSealSigner(h, seal)
			},
			CommitBlock: func(h *types.Header, seals [][]byte) (*types.Header, error) {
				return bftEng.CommitBlock(h, seals)
			},
		})

		// Drain stale messages from previous rounds.
		for {
			select {
			case <-n.qbftMsgCh:
			default:
				goto drained
			}
		}
	drained:

		var initialDecisions []qbftcore.Decision
		var builtPayload *engine.ExecutionPayloadV3

		if isProposer {
			h, ep, err := n.buildQBFTProposal(ctx, parent, snap, nextNum)
			if err != nil {
				if ctx.Err() != nil {
					return nil, nil, nil
				}
				return nil, nil, fmt.Errorf("build proposal: %w", err)
			}
			payloadJSON, err := json.Marshal(ep)
			if err != nil {
				return nil, nil, fmt.Errorf("marshal payload: %w", err)
			}
			builtPayload = ep
			initialDecisions = core.StartProposer(h, payloadJSON)
		}

		if len(initialDecisions) > 0 {
			result, err := n.dispatchQBFTDecisions(ctx, initialDecisions)
			if err != nil {
				return nil, nil, err
			}
			if result.CommittedHeader != nil {
				payload := result.CommittedPayload
				if payload == nil {
					payload = builtPayload
				}
				return result.CommittedHeader, payload, nil
			}
		}

		// Main round event loop.
		timer := time.NewTimer(timeout)
		result, err := n.qbftRoundLoop(ctx, core, timer)
		timer.Stop()

		if err != nil {
			return nil, nil, err
		}
		if ctx.Err() != nil {
			return nil, nil, nil
		}
		if result != nil && result.CommittedHeader != nil {
			return result.CommittedHeader, result.CommittedPayload, nil
		}
		// StartRound: continue to next round iteration.
		_ = builtPayload
	}
}

// qbftDecisionResult is returned by dispatchQBFTDecisions and qbftRoundLoop.
type qbftDecisionResult struct {
	CommittedHeader  *types.Header
	CommittedPayload *engine.ExecutionPayloadV3
	// StartRound is true when the core returned a StartRound decision.
	StartRound bool
}

// qbftRoundLoop runs the select loop for one round until a terminal decision
// is reached (CommitBlock or StartRound) or ctx is cancelled.
func (n *Node) qbftRoundLoop(
	ctx context.Context,
	core *qbftcore.Core,
	timer *time.Timer,
) (*qbftDecisionResult, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, nil

		case rawMsg := <-n.qbftMsgCh:
			from, err := recoverQBFTMsgSender(rawMsg)
			if err != nil {
				n.log.Debug().Err(err).Msg("qbft: invalid message signature, discarding")
				continue
			}
			incoming := qbftcore.IncomingMsg{
				MsgType: qbftcore.MsgType(rawMsg.Type),
				From:    from,
				Data:    rawMsg.Data,
			}
			decisions := core.HandleMsg(incoming)
			result, err := n.dispatchQBFTDecisions(ctx, decisions)
			if err != nil {
				return nil, err
			}
			if result != nil && (result.CommittedHeader != nil || result.StartRound) {
				return result, nil
			}

		case <-timer.C:
			decisions := core.Timeout()
			result, err := n.dispatchQBFTDecisions(ctx, decisions)
			if err != nil {
				return nil, err
			}
			if result != nil && (result.CommittedHeader != nil || result.StartRound) {
				return result, nil
			}
		}
	}
}

// dispatchQBFTDecisions processes the decisions returned by the core state
// machine and performs the corresponding actions (broadcast, commit, etc.).
func (n *Node) dispatchQBFTDecisions(
	ctx context.Context,
	decisions []qbftcore.Decision,
) (*qbftDecisionResult, error) {
	for _, d := range decisions {
		switch d.Type {
		case qbftcore.Broadcast:
			if n.p2p == nil || n.signerKey == nil {
				continue
			}
			msg := &p2phost.QBFTMsg{
				Type: uint8(d.MsgType),
				Data: d.MsgData,
			}
			if err := signQBFTMsg(msg, n.signerKey); err != nil {
				n.log.Warn().Err(err).Msg("qbft: sign message failed")
				continue
			}
			if err := n.p2p.BroadcastQBFTMsg(ctx, msg); err != nil {
				n.log.Warn().Err(err).Msg("qbft: broadcast failed")
			}
			// Echo back to ourselves so our own messages are counted.
			select {
			case n.qbftMsgCh <- msg:
			default:
			}

		case qbftcore.CommitBlock:
			if d.Header == nil {
				continue
			}
			var ep *engine.ExecutionPayloadV3
			var elHash common.Hash
			if len(d.Payload) > 0 {
				ep = new(engine.ExecutionPayloadV3)
				if err := json.Unmarshal(d.Payload, ep); err != nil {
					return nil, fmt.Errorf("qbft: unmarshal payload: %w", err)
				}
				elHash = ep.BlockHash
				if err := n.importPayload(ctx, *ep); err != nil {
					n.log.Error().Err(err).Msg("qbft: importPayload failed")
				}
			}

			headChanged, _, err := n.stor.AddBlock(d.Header, elHash, d.Payload)
			if err != nil {
				return nil, fmt.Errorf("qbft: AddBlock: %w", err)
			}
			if headChanged {
				state := n.stor.ForkchoiceState()
				if _, err := n.eng.ForkchoiceUpdatedV3(ctx, state, nil); err != nil {
					n.log.Warn().Err(err).Msg("qbft: FCU after commit failed")
				}
			}

			// Propagate the committed block to peers via the block gossip topic.
			if n.p2p != nil && ep != nil {
				blk, err := p2phost.NewCliqueBlock(d.Header, *ep)
				if err == nil {
					if err := n.p2p.BroadcastBlock(ctx, blk); err != nil {
						n.log.Warn().Err(err).Msg("qbft: broadcast committed block failed")
					}
				}
			}

			return &qbftDecisionResult{
				CommittedHeader:  d.Header,
				CommittedPayload: ep,
			}, nil

		case qbftcore.StartRound:
			return &qbftDecisionResult{StartRound: true}, nil
		}
	}
	return nil, nil
}

// buildQBFTProposal builds the next block header and fetches the execution
// payload from the EL. Called by the proposer at the start of each instance.
func (n *Node) buildQBFTProposal(
	ctx context.Context,
	parent *types.Header,
	snap consensus.Snapshot,
	nextNum uint64,
) (*types.Header, *engine.ExecutionPayloadV3, error) {
	targetTime := parent.Time + n.cliq.Period()
	now := uint64(time.Now().Unix())
	if now < targetTime {
		select {
		case <-time.After(time.Duration(targetTime-now) * time.Second):
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}

	elExtra := make([]byte, n.cliq.ExtraVanity())
	clExtra := n.cliq.BuildExtra(snap, nextNum)

	zeroHash := common.Hash{}
	attrs := &engine.PayloadAttributesV3{
		Timestamp:             hexutil.Uint64(targetTime),
		PrevRandao:            common.Hash{},
		SuggestedFeeRecipient: n.signerAddr,
		Withdrawals:           []*engine.Withdrawal{},
		ParentBeaconBlockRoot: &zeroHash,
		ExtraData:             elExtra,
	}

	state := n.stor.ForkchoiceState()
	fcuResult, err := n.eng.ForkchoiceUpdatedV3(ctx, state, attrs)
	if err != nil {
		return nil, nil, fmt.Errorf("FCU: %w", err)
	}
	if fcuResult.PayloadStatus.Status != engine.PayloadStatusValid {
		return nil, nil, fmt.Errorf("FCU returned status %s", fcuResult.PayloadStatus.Status)
	}
	if fcuResult.PayloadID == nil {
		return nil, nil, fmt.Errorf("FCU returned no payloadId")
	}

	payloadResp, err := n.eng.GetPayloadV3(ctx, *fcuResult.PayloadID)
	if err != nil {
		return nil, nil, fmt.Errorf("GetPayload: %w", err)
	}
	ep := payloadResp.ExecutionPayload

	var baseFee *big.Int
	if ep.BaseFeePerGas != nil {
		baseFee = ep.BaseFeePerGas.ToInt()
	}

	header := &types.Header{
		ParentHash:  parent.Hash(),
		UncleHash:   n.cliq.EmptyUncleHash(),
		Coinbase:    common.Address{},
		Root:        ep.StateRoot,
		ReceiptHash: ep.ReceiptsRoot,
		Bloom:       types.BytesToBloom(ep.LogsBloom),
		Difficulty:  n.cliq.CalcDifficulty(snap, nextNum, n.signerAddr),
		Number:      new(big.Int).SetUint64(nextNum),
		GasLimit:    uint64(ep.GasLimit),
		GasUsed:     uint64(ep.GasUsed),
		Time:        uint64(ep.Timestamp),
		Extra:       clExtra,
		MixDigest:   common.Hash{},
		Nonce:       n.cliq.NonceDrop(),
		BaseFee:     baseFee,
	}

	if err := n.cliq.SealHeader(header, n.signerKey); err != nil {
		return nil, nil, fmt.Errorf("SealHeader: %w", err)
	}

	return header, &ep, nil
}

// isQBFTProposerForRound returns true if this node is the designated proposer
// for the given block number and round. The proposer rotates: round-r proposer
// is validators[(number + round) % N].
func (n *Node) isQBFTProposerForRound(snap consensus.Snapshot, number uint64, round uint32) bool {
	if n.signerKey == nil {
		return false
	}
	validators := snap.SignerList()
	if len(validators) == 0 {
		return false
	}
	idx := (int(number) + int(round)) % len(validators)
	return validators[idx] == n.signerAddr
}

// signQBFTMsg writes a 65-byte ECDSA signature into m.Sig.
// The signed data is keccak256(RLP([m.Type, m.Data])).
func signQBFTMsg(m *p2phost.QBFTMsg, key *ecdsa.PrivateKey) error {
	hash := qbftMsgHash(m)
	sig, err := gethcrypto.Sign(hash.Bytes(), key)
	if err != nil {
		return fmt.Errorf("sign qbft msg: %w", err)
	}
	m.Sig = sig
	return nil
}

// recoverQBFTMsgSender recovers the sender address from a signed QBFTMsg.
func recoverQBFTMsgSender(m *p2phost.QBFTMsg) (common.Address, error) {
	if len(m.Sig) != 65 {
		return common.Address{}, fmt.Errorf("invalid signature length %d", len(m.Sig))
	}
	hash := qbftMsgHash(m)
	pubkey, err := gethcrypto.Ecrecover(hash.Bytes(), m.Sig)
	if err != nil {
		return common.Address{}, fmt.Errorf("ecrecover: %w", err)
	}
	if len(pubkey) == 0 || pubkey[0] != 4 {
		return common.Address{}, fmt.Errorf("invalid public key")
	}
	var addr common.Address
	copy(addr[:], gethcrypto.Keccak256(pubkey[1:])[12:])
	return addr, nil
}

// qbftMsgHash returns keccak256(RLP([Type, Data])) for signing/verification.
func qbftMsgHash(m *p2phost.QBFTMsg) common.Hash {
	enc, err := rlp.EncodeToBytes([]interface{}{m.Type, m.Data})
	if err != nil {
		panic("qbft: rlp encode for hash: " + err.Error())
	}
	return gethcrypto.Keccak256Hash(enc)
}
