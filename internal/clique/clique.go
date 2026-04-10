// Package clique implements the Clique proof-of-authority consensus mechanism
// described in EIP-225. In Clique, a fixed set of authorized signers take turns
// producing blocks. Signer membership is managed through an on-chain voting
// mechanism embedded in the block header's coinbase and nonce fields.
//
// This package provides:
//   - Header verification (VerifyHeader)
//   - Block sealing / signer recovery (SealHeader, SignerFromHeader)
//   - Signer state tracking via snapshots (Snapshot, Apply)
//   - Difficulty calculation (CalcDifficulty)
//
// The package does NOT manage a chain or a snapshot cache; those concerns
// belong to the node orchestrator (Phase 4 fork choice).
package clique

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// Extra-data field layout (EIP-225):
//
//	[32 bytes vanity][optional: N×20 bytes signers at epoch][65 bytes ECDSA seal]
const (
	extraVanity = 32 // bytes reserved for signer vanity at the start of Extra
	extraSeal   = 65 // bytes for the signer's ECDSA signature at the end of Extra
)

// Clique block difficulty values.
const (
	diffInTurn = 2 // difficulty when signing in turn (lower latency, canonical chain)
	diffNoTurn = 1 // difficulty when signing out of turn
)

// Nonce magic values for voting (stored as big-endian uint64 in BlockNonce).
const (
	nonceAuthUint64 = uint64(0xffffffffffffffff) // vote to authorize a new signer
	nonceDropUint64 = uint64(0x0000000000000000) // vote to remove a signer (also "no vote")
)

// emptyUncleHash is the keccak256 of the RLP encoding of an empty uncle list,
// required in every Clique block header.
var emptyUncleHash = types.CalcUncleHash(nil)

// nonceAuth and nonceNull as BlockNonce values for direct comparison.
var (
	nonceAuth = types.BlockNonce{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	nonceNull = types.BlockNonce{} // all-zero nonce
)

// Sentinel errors returned by the Clique engine.
var (
	ErrUnauthorizedSigner = errors.New("unauthorized signer")
	ErrRecentlySigned     = errors.New("signed recently, must wait for others")
	ErrInvalidExtraData   = errors.New("invalid extra data length")
	ErrInvalidNonce       = errors.New("nonce must be 0x0000000000000000 or 0xffffffffffffffff")
	ErrInvalidVoteAddress = errors.New("authorize-vote nonce requires a non-zero beneficiary")
	ErrInvalidDifficulty  = errors.New("difficulty must be 1 (out-of-turn) or 2 (in-turn)")
	ErrWrongDifficulty    = errors.New("wrong difficulty for signer's turn position")
	ErrInvalidMixDigest   = errors.New("mix digest must be the zero hash")
	ErrInvalidUncleHash   = errors.New("uncle hash must be keccak256 of the empty uncle list")
	ErrInvalidTimestamp   = errors.New("block timestamp is too early")
)

// Engine implements Clique proof-of-authority consensus (EIP-225).
type Engine struct {
	// period is the minimum seconds between blocks (genesis.config.clique.period).
	period uint64
	// epoch is the blocks per epoch: vote reset + signer checkpoint interval
	// (genesis.config.clique.epoch).
	epoch uint64
}

// New creates a new Clique Engine with the given block period and epoch length.
func New(period, epoch uint64) *Engine {
	return &Engine{period: period, epoch: epoch}
}

// Period returns the configured minimum block period in seconds.
func (e *Engine) Period() uint64 { return e.period }

// Epoch returns the configured epoch length in blocks.
func (e *Engine) Epoch() uint64 { return e.epoch }

// CalcDifficulty returns the expected block difficulty for signer at the given
// block number: diffInTurn (2) when it is their designated turn, diffNoTurn (1)
// otherwise.
func (e *Engine) CalcDifficulty(snap *Snapshot, number uint64, signer common.Address) *big.Int {
	if snap.InTurn(number, signer) {
		return big.NewInt(diffInTurn)
	}
	return big.NewInt(diffNoTurn)
}

// Apply creates a new snapshot by replaying headers on top of snap. The headers
// must be consecutive blocks starting immediately after snap.Number.
func (e *Engine) Apply(snap *Snapshot, headers []*types.Header) (*Snapshot, error) {
	return snap.apply(headers, e.epoch)
}

// VerifyHeader checks that header conforms to the Clique rules given the
// authorization snapshot at the parent block and the parent header itself.
//
// Checks performed (in order):
//  1. Extra data length (epoch vs non-epoch)
//  2. Uncle hash
//  3. Nonce validity
//  4. Mix digest
//  5. Difficulty range
//  6. Block timestamp >= parent.Time + period
//  7. Signer is authorized and not in the recent-signer window
//  8. Difficulty matches the signer's in-turn/out-of-turn position
//
// parent may be nil only for genesis (number 0), which should not be passed
// to this function in normal operation.
func (e *Engine) VerifyHeader(snap *Snapshot, header *types.Header, parent *types.Header) error {
	number := header.Number.Uint64()

	// 1. Extra data length.
	// All blocks: at least extraVanity + extraSeal bytes.
	// Epoch blocks: the middle section must be a non-empty multiple of 20 (address size).
	// Non-epoch blocks: no middle section (exactly extraVanity + extraSeal bytes).
	if len(header.Extra) < extraVanity+extraSeal {
		return fmt.Errorf("%w: have %d, minimum is %d",
			ErrInvalidExtraData, len(header.Extra), extraVanity+extraSeal)
	}
	signerBytes := len(header.Extra) - extraVanity - extraSeal
	if number%e.epoch == 0 {
		if signerBytes == 0 || signerBytes%common.AddressLength != 0 {
			return fmt.Errorf(
				"%w: epoch block has %d signer bytes (must be a non-zero multiple of %d)",
				ErrInvalidExtraData, signerBytes, common.AddressLength)
		}
	} else {
		if signerBytes != 0 {
			return fmt.Errorf(
				"%w: non-epoch block has %d extra signer bytes (must be 0)",
				ErrInvalidExtraData, signerBytes)
		}
	}

	// 2. Uncle hash.
	if header.UncleHash != emptyUncleHash {
		return ErrInvalidUncleHash
	}

	// 3. Nonce: must be all-zeroes or all-ones.
	if header.Nonce != nonceAuth && header.Nonce != nonceNull {
		return ErrInvalidNonce
	}
	// An authorize-vote nonce requires a non-zero beneficiary address.
	if header.Nonce == nonceAuth && header.Coinbase == (common.Address{}) {
		return ErrInvalidVoteAddress
	}

	// 4. Mix digest must be zero.
	if header.MixDigest != (common.Hash{}) {
		return ErrInvalidMixDigest
	}

	// 5. Difficulty must be 1 or 2.
	if header.Difficulty == nil {
		return ErrInvalidDifficulty
	}
	d := header.Difficulty.Int64()
	if d != diffInTurn && d != diffNoTurn {
		return fmt.Errorf("%w: have %v", ErrInvalidDifficulty, header.Difficulty)
	}

	// 6. Timestamp.
	if parent != nil {
		minTime := parent.Time + e.period
		if header.Time < minTime {
			return fmt.Errorf("%w: have %d, need >= %d",
				ErrInvalidTimestamp, header.Time, minTime)
		}
	}

	// 7. Signer authorization and recent-signer check.
	signer, err := SignerFromHeader(header)
	if err != nil {
		return fmt.Errorf("recover signer: %w", err)
	}
	if !snap.IsAuthorized(signer) {
		return fmt.Errorf("%w: %s", ErrUnauthorizedSigner, signer)
	}
	if snap.HasRecentlySigned(number, signer) {
		return fmt.Errorf("%w: %s", ErrRecentlySigned, signer)
	}

	// 8. Difficulty must match the signer's turn position.
	expected := e.CalcDifficulty(snap, number, signer)
	if header.Difficulty.Cmp(expected) != 0 {
		return fmt.Errorf("%w: have %v, want %v",
			ErrWrongDifficulty, header.Difficulty, expected)
	}

	return nil
}
