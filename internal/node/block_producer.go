package node

import (
	"context"
	"encoding/json"
	"math/big"
	"math/rand"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"

	cliqueeng "github.com/peterrobinson/consensus-client-vibe/internal/clique"
	"github.com/peterrobinson/consensus-client-vibe/internal/engine"
	p2phost "github.com/peterrobinson/consensus-client-vibe/internal/p2p"
)

const wiggle = 500 * time.Millisecond // per-slot extra delay for out-of-turn signers

// scheduleBlockProduction resets the production timer based on the current
// head. It is called after every head change (from P2P or from production).
// It is a no-op in follower mode (no signer key) or if we are not in the
// current authorized signer set.
func (n *Node) scheduleBlockProduction(ctx context.Context) {
	if n.signerKey == nil {
		return
	}

	snap := n.headSnapshot()
	if snap == nil {
		return
	}
	head := n.stor.Head()
	if head == nil {
		return
	}

	// Check we are authorized.
	if !snap.IsAuthorized(n.signerAddr) {
		n.log.Debug().Msg("not in current signer set, skipping production")
		return
	}

	signers := snap.SignerList()
	nextNum := head.Number.Uint64() + 1
	inTurnIdx := int(nextNum % uint64(len(signers)))

	// Find our index in the signer list.
	ourIdx := -1
	for i, s := range signers {
		if s == n.signerAddr {
			ourIdx = i
			break
		}
	}
	if ourIdx < 0 {
		return // shouldn't happen since IsAuthorized passed
	}

	// Compute distance from in-turn position (positive, wrapping).
	dist := ourIdx - inTurnIdx
	if dist < 0 {
		dist += len(signers)
	}

	// Base firing time: parent.Time + period.
	baseTime := time.Unix(int64(head.Time), 0).Add(
		time.Duration(n.cfg.Clique.Period) * time.Second)

	var delay time.Duration
	if dist == 0 {
		// In-turn: fire exactly at baseTime.
		delay = time.Until(baseTime)
	} else {
		// Out-of-turn: back off by dist×wiggle + a random jitter up to wiggle.
		// Floor time.Until(baseTime) at zero so that when the parent block is
		// old (e.g. genesis timestamp = 0), the wiggle is still measured from
		// now rather than from a large negative value that would clamp every
		// signer's delay to zero, causing simultaneous production.
		//nolint:gosec // non-crypto random is fine for block timing
		jitter := time.Duration(rand.Int63n(int64(wiggle)))
		fromBase := time.Until(baseTime)
		if fromBase < 0 {
			fromBase = 0
		}
		delay = fromBase + time.Duration(dist)*wiggle + jitter
	}
	if delay < 0 {
		delay = 0
	}

	n.log.Debug().
		Uint64("next_block", nextNum).
		Int("our_idx", ourIdx).
		Int("inturn_idx", inTurnIdx).
		Dur("delay", delay).
		Msg("scheduling block production")

	// Cancel any previous timer and start a fresh one.
	// Always derive from n.runCtx (the node lifecycle context), NOT from the
	// caller's ctx. If ctx were a prodCtx from a previous slot,
	// scheduleBlockProduction would cancel it below, immediately killing the
	// child context for the next slot.
	// Fall back to ctx if runCtx has not been set (e.g. in unit tests that
	// call scheduleBlockProduction directly without going through Start).
	rootCtx := n.runCtx
	if rootCtx == nil {
		rootCtx = ctx
	}
	prodCtx, cancel := context.WithCancel(rootCtx)

	n.prodMu.Lock()
	if n.prodTimer != nil {
		n.prodTimer.cancel()
	}
	n.prodTimer = &timerCancel{cancel: cancel}
	n.prodMu.Unlock()

	go func() {
		select {
		case <-time.After(delay):
			n.produceBlock(prodCtx)
		case <-prodCtx.Done():
		}
	}()
}

// produceBlock executes the full block-production pipeline when it is this
// node's turn (or close enough to it after accounting for timing skew):
//
//  1. Sanity-check the signer is still authorized and hasn't signed too recently.
//  2. Request a payload from the EL (engine_forkchoiceUpdated with payloadAttributes).
//  3. Fetch the completed payload (engine_getPayload).
//  4. Build the Clique header from the execution payload fields.
//  5. Seal the header with the signer key.
//  6. Broadcast the block to peers via Gossipsub.
//  7. Import the payload into the local EL (engine_newPayload).
//  8. Update the local fork choice (engine_forkchoiceUpdated).
//  9. Advance the in-memory head and reschedule for the next slot.
func (n *Node) produceBlock(ctx context.Context) {
	snap := n.headSnapshot()
	if snap == nil {
		return
	}
	head := n.stor.Head()
	if head == nil {
		return
	}

	nextNum := head.Number.Uint64() + 1

	// Guard: still authorized?
	if !snap.IsAuthorized(n.signerAddr) {
		n.log.Debug().Msg("produceBlock: no longer authorized")
		return
	}
	// Guard: signed too recently?
	if snap.HasRecentlySigned(nextNum, n.signerAddr) {
		n.log.Debug().Msg("produceBlock: signed recently, skipping")
		n.log.Debug().
      		Str("signer", n.signerAddr.Hex()).
      		Uint64("value", nextNum).
      		Msg("produceBlock: signed recently")          
		return
	}

	targetTime := head.Time + n.cfg.Clique.Period
	// Don't produce significantly before the target time.
	if uint64(time.Now().Unix())+1 < targetTime {
		n.log.Debug().
			Uint64("target", targetTime).
			Int64("now", time.Now().Unix()).
			Msg("produceBlock: too early, rescheduling")
		n.scheduleBlockProduction(ctx)
		return
	}

	n.log.Info().Uint64("number", nextNum).Msg("producing block")

	// Step 2: Request payload from EL.
	//
	// elExtra is the extraData written into the EL block — a plain 32-byte
	// vanity field. Post-merge Geth enforces ≤32 bytes on extraData, so we
	// cannot embed the Clique seal here.
	//
	// clExtra is the full Clique Extra for the CL header: vanity + optional
	// epoch signer list + 65-byte seal placeholder (filled by SealHeader).
	// The seal lives in the CL header only; peers recover the signer from it.
	elExtra := make([]byte, cliqueeng.ExtraVanity)
	clExtra := n.buildExtra(snap, nextNum)

	zeroHash := common.Hash{}
	attrs := &engine.PayloadAttributesV3{
		Timestamp:             hexutil.Uint64(targetTime),
		PrevRandao:            common.Hash{},
		SuggestedFeeRecipient: n.signerAddr,
		Withdrawals:           []*engine.Withdrawal{},
		ParentBeaconBlockRoot: &zeroHash, //nolint:gosec
		ExtraData:             elExtra,
	}
	// The EL on this node may not yet have the parent execution payload: the
	// CL wire message carries only the CL header, so peer ELs must sync the
	// payload from devp2p, which can take hundreds of ms. Poll until VALID or
	// until the sync timeout expires, then reschedule rather than silently die.
	const elSyncTimeout = 8 * time.Second
	const elSyncPoll = 300 * time.Millisecond

	state := n.stor.ForkchoiceState()
	fcuResult, err := n.eng.ForkchoiceUpdatedV3(ctx, state, attrs)
	if err != nil {
		n.log.Error().Err(err).Msg("produceBlock: ForkchoiceUpdatedV3 (start) failed")
		return
	}
	syncDeadline := time.Now().Add(elSyncTimeout)
	for fcuResult.PayloadStatus.Status == engine.PayloadStatusSyncing {
		if time.Now().After(syncDeadline) {
			n.log.Warn().Uint64("number", nextNum).
				Msg("produceBlock: EL still syncing parent, rescheduling")
			n.scheduleBlockProduction(ctx)
			return
		}
		n.log.Debug().Uint64("number", nextNum).
			Msg("produceBlock: EL syncing parent, waiting")
		select {
		case <-time.After(elSyncPoll):
		case <-ctx.Done():
			return
		}
		fcuResult, err = n.eng.ForkchoiceUpdatedV3(ctx, state, attrs)
		if err != nil {
			n.log.Error().Err(err).Msg("produceBlock: ForkchoiceUpdatedV3 (retry) failed")
			return
		}
	}
	if fcuResult.PayloadStatus.Status != engine.PayloadStatusValid {
		n.log.Error().Str("status", fcuResult.PayloadStatus.Status).
			Msg("produceBlock: FCU returned non-VALID status")
		return
	}
	if fcuResult.PayloadID == nil {
		n.log.Error().Msg("produceBlock: no payloadId returned")
		return
	}

	// Step 3: Fetch the built payload.
	payloadResp, err := n.eng.GetPayloadV3(ctx, *fcuResult.PayloadID)
	if err != nil {
		n.log.Error().Err(err).Msg("produceBlock: GetPayloadV3 failed")
		return
	}
	ep := payloadResp.ExecutionPayload

	// Step 4: Build Clique header from the execution payload fields.
	// We use our own extra data (which includes the Clique vanity / epoch
	// signer list + 65 zero bytes for the seal) rather than whatever the EL
	// put in its version of the block's extra data.
	var baseFee *big.Int
	if ep.BaseFeePerGas != nil {
		baseFee = ep.BaseFeePerGas.ToInt()
	}
	nonce := cliqueeng.NonceDrop
	coinbase := n.signerAddr

	// Apply pending vote if one was set via POST /clique/v1/vote.
	if n.rpc != nil {
		if vote := n.rpc.PendingVote(); vote != nil {
			coinbase = vote.Address
			if vote.Authorize {
				nonce = cliqueeng.NonceAuth
			}
		}
	}

	// The CL header's ParentHash is the CL hash of the parent block, which is
	// what the store uses for all lookups and snapshot computation.
	// The EL payload's parent is tracked by the EL via the FCU HeadBlockHash
	// (which ForkchoiceState now returns as the EL hash).
	header := &types.Header{
		ParentHash:  head.Hash(),
		UncleHash:   cliqueeng.EmptyUncleHash,
		Coinbase:    coinbase,
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
		Nonce:       nonce,
		BaseFee:     baseFee,
	}

	// Step 5: Seal the CL header (writes 65-byte ECDSA signature into Extra).
	// The EL execution payload (ep) is not modified — it keeps the 32-byte
	// elExtra and its own block hash (ep.BlockHash) from Geth.
	if err := cliqueeng.SealHeader(header, n.signerKey); err != nil {
		n.log.Error().Err(err).Msg("produceBlock: SealHeader failed")
		return
	}

	// Step 6: Broadcast to peers.
	// The full execution payload is included so receiving nodes can deliver it
	// to their local EL via engine_newPayloadV3 without waiting for devp2p.
	blk, err := p2phost.NewCliqueBlock(header, ep)
	if err != nil {
		n.log.Error().Err(err).Msg("produceBlock: NewCliqueBlock failed")
		return
	}
	if n.p2p != nil {
		if err := n.p2p.BroadcastBlock(ctx, blk); err != nil {
			n.log.Warn().Err(err).Msg("produceBlock: BroadcastBlock failed")
		}
	}

	// Step 7: Import into local EL.
	if err := n.importPayload(ctx, ep); err != nil {
		n.log.Error().Err(err).Msg("produceBlock: importPayload failed")
		return
	}

	// Step 8: Add to store, update fork choice.
	// Pass ep.BlockHash as the EL hash so ForkchoiceState returns the correct
	// EL block hash to engine_forkchoiceUpdated. Also persist the payload JSON
	// so the sync protocol can deliver it to peers' execution clients.
	epJSON, _ := json.Marshal(ep)
	headChanged, err := n.stor.AddBlock(header, ep.BlockHash, epJSON)
	if err != nil {
		n.log.Error().Err(err).Msg("produceBlock: AddBlock failed")
		return
	}
	if headChanged {
		newState := n.stor.ForkchoiceState()
		if _, err := n.eng.ForkchoiceUpdatedV3(ctx, newState, nil); err != nil {
			n.log.Error().Err(err).Msg("produceBlock: ForkchoiceUpdatedV3 (new head) failed")
		}
	}

	// Step 9: Advance snapshot and reschedule.
	newSnap, err := n.cliq.Apply(snap, []*types.Header{header})
	if err != nil {
		n.log.Error().Err(err).Msg("produceBlock: Apply snapshot failed")
	} else {
		n.setHeadSnapshot(newSnap)
	}

	blockHash := header.Hash()
	if n.p2p != nil {
		n.p2p.SetStatus(p2phost.StatusMsg{
			NetworkID:   n.cfg.Node.NetworkID,
			GenesisHash: n.genesisSnap.Hash,
			HeadHash:    blockHash,
			HeadNumber:  nextNum,
		})
	}

	n.log.Info().
		Uint64("number", nextNum).
		Str("hash", blockHash.Hex()).
		Str("signer", n.signerAddr.Hex()).
		Msg("block produced and imported")

	n.scheduleBlockProduction(ctx)
}

// buildExtra constructs the Extra field for the next Clique block.
// At epoch boundaries, the signer list is embedded after the vanity bytes.
// The last ExtraSeal bytes are zero-padded (to be filled in by SealHeader).
func (n *Node) buildExtra(snap *cliqueeng.Snapshot, nextNum uint64) []byte {
	extra := make([]byte, cliqueeng.ExtraVanity) // 32 zero vanity bytes
	if nextNum%n.cfg.Clique.Epoch == 0 {
		for _, addr := range snap.SignerList() {
			extra = append(extra, addr.Bytes()...)
		}
	}
	extra = append(extra, make([]byte, cliqueeng.ExtraSeal)...) // 65 zero seal bytes
	return extra
}
