package qbft

import (
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/peterrobinson/consensus-client-vibe/internal/consensus"
)

var emptyUncleHash = types.CalcUncleHash(nil)

var (
	nonceNull = types.BlockNonce{}
)

// Sentinel errors.
var (
	ErrUnauthorizedValidator = fmt.Errorf("qbft: unauthorized validator")
	ErrInvalidExtraData      = fmt.Errorf("qbft: invalid extra data")
	ErrInvalidUncleHash      = fmt.Errorf("qbft: uncle hash must be empty uncle hash")
	ErrInvalidDifficulty     = fmt.Errorf("qbft: difficulty must be 1")
	ErrInvalidTimestamp      = fmt.Errorf("qbft: block timestamp is too early")
	ErrInsufficientSeals     = fmt.Errorf("qbft: insufficient committed seals")
)

// Engine implements QBFT consensus. It satisfies both consensus.Engine and
// consensus.BFTEngine.
type Engine struct {
	period         uint64
	epoch          uint64
	requestTimeout time.Duration
}

// New creates a new QBFT Engine.
func New(period, epoch uint64, timeout time.Duration) *Engine {
	return &Engine{period: period, epoch: epoch, requestTimeout: timeout}
}

// --- consensus.Engine methods ---

// Period returns the configured minimum block period in seconds.
func (e *Engine) Period() uint64 { return e.period }

// Epoch returns the configured epoch length in blocks.
func (e *Engine) Epoch() uint64 { return e.epoch }

// ExtraVanity returns the fixed byte count of the vanity prefix.
func (e *Engine) ExtraVanity() int { return ExtraVanity }

// ExtraSeal returns the fixed byte count of the ECDSA seal.
func (e *Engine) ExtraSeal() int { return ExtraSeal }

// EmptyUncleHash returns the required UncleHash for all QBFT blocks.
func (e *Engine) EmptyUncleHash() common.Hash { return emptyUncleHash }

// NonceAuth returns a zero nonce. QBFT does not use nonce-based on-chain voting.
func (e *Engine) NonceAuth() types.BlockNonce { return nonceNull }

// NonceDrop returns a zero nonce.
func (e *Engine) NonceDrop() types.BlockNonce { return nonceNull }

// NewGenesisSnapshot derives the initial QBFT snapshot from the genesis header.
// The genesis header must contain a valid QBFT Extra with a non-empty validator list.
func (e *Engine) NewGenesisSnapshot(genesis *types.Header) (consensus.Snapshot, error) {
	ie, err := DecodeExtra(genesis)
	if err != nil {
		return nil, fmt.Errorf("qbft genesis snapshot: %w", err)
	}
	if len(ie.Validators) == 0 {
		return nil, fmt.Errorf("qbft genesis snapshot: empty validator list in genesis extra")
	}
	return newSnapshot(0, genesis.Hash(), ie.Validators), nil
}

// NewCheckpointSnapshot derives a QBFT snapshot from an epoch checkpoint header.
func (e *Engine) NewCheckpointSnapshot(header *types.Header) (consensus.Snapshot, error) {
	ie, err := DecodeExtra(header)
	if err != nil {
		return nil, fmt.Errorf("qbft checkpoint snapshot: %w", err)
	}
	if len(ie.Validators) == 0 {
		return nil, fmt.Errorf("qbft checkpoint snapshot: empty validator list at block %d", header.Number)
	}
	return newSnapshot(header.Number.Uint64(), header.Hash(), ie.Validators), nil
}

// CalcDifficulty always returns 1. Difficulty is meaningless in QBFT; blocks
// are immediately final so there is never a competing fork.
func (e *Engine) CalcDifficulty(_ consensus.Snapshot, _ uint64, _ common.Address) *big.Int {
	return big.NewInt(1)
}

// Apply creates a new snapshot by replaying headers onto snap.
func (e *Engine) Apply(snap consensus.Snapshot, headers []*types.Header) (consensus.Snapshot, error) {
	s, ok := snap.(*Snapshot)
	if !ok {
		return nil, fmt.Errorf("qbft: Apply requires a *Snapshot, got %T", snap)
	}
	return s.apply(headers, e.epoch)
}

// VerifyHeader checks that header conforms to QBFT rules given the snapshot at
// the parent block. It validates structural fields and checks that the committed
// seals satisfy quorum. For proposal headers (CommittedSeals=nil), seal
// verification is skipped — use VerifyProposal for those.
func (e *Engine) VerifyHeader(snap consensus.Snapshot, header *types.Header, parent *types.Header) error {
	number := header.Number.Uint64()

	// Uncle hash.
	if header.UncleHash != emptyUncleHash {
		return ErrInvalidUncleHash
	}

	// Difficulty must be 1.
	if header.Difficulty == nil || header.Difficulty.Cmp(big.NewInt(1)) != 0 {
		return ErrInvalidDifficulty
	}

	// Timestamp.
	if parent != nil {
		minTime := parent.Time + e.period
		if header.Time < minTime {
			return fmt.Errorf("%w: have %d, need >= %d", ErrInvalidTimestamp, header.Time, minTime)
		}
	}

	// Extra data.
	if len(header.Extra) < ExtraVanity {
		return fmt.Errorf("%w: too short at block %d", ErrInvalidExtraData, number)
	}
	ie, err := DecodeExtra(header)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidExtraData, err)
	}

	// Proposer must be authorized.
	proposer, err := SignerFromHeader(header)
	if err != nil {
		return fmt.Errorf("qbft: recover proposer: %w", err)
	}
	if !snap.IsAuthorized(proposer) {
		return fmt.Errorf("%w: %s", ErrUnauthorizedValidator, proposer)
	}

	// Committed seals: verify quorum.
	validators := snap.SignerList()
	quorum := e.Quorum(len(validators))
	if len(ie.CommittedSeals) < quorum {
		return fmt.Errorf("%w: have %d, need %d", ErrInsufficientSeals, len(ie.CommittedSeals), quorum)
	}
	seen := make(map[common.Address]struct{})
	for _, seal := range ie.CommittedSeals {
		signer, err := RecoverCommitSealSigner(header, seal)
		if err != nil {
			return fmt.Errorf("qbft: recover commit seal: %w", err)
		}
		if !snap.IsAuthorized(signer) {
			return fmt.Errorf("%w: commit seal from %s", ErrUnauthorizedValidator, signer)
		}
		if _, dup := seen[signer]; dup {
			return fmt.Errorf("qbft: duplicate commit seal from %s", signer)
		}
		seen[signer] = struct{}{}
	}

	return nil
}

// SealHeader writes the proposer ECDSA seal into header's IstanbulExtra.
func (e *Engine) SealHeader(header *types.Header, key *ecdsa.PrivateKey) error {
	return SealHeader(header, key)
}

// SignerFromHeader recovers the proposer address from a sealed QBFT header.
func (e *Engine) SignerFromHeader(header *types.Header) (common.Address, error) {
	return SignerFromHeader(header)
}

// BuildExtra constructs the CL-side Extra field for the next QBFT block.
// At epoch boundaries, the current validator set is embedded in IstanbulExtra.
func (e *Engine) BuildExtra(snap consensus.Snapshot, number uint64) []byte {
	ie := &IstanbulExtra{
		Round:          0,
		Seal:           nil,
		CommittedSeals: nil,
	}
	if number%e.epoch == 0 {
		ie.Validators = snap.SignerList()
	}
	vanity := make([]byte, ExtraVanity)
	extra, err := EncodeExtra(vanity, ie)
	if err != nil {
		// EncodeExtra only fails on RLP encoding errors which cannot happen with
		// the simple types used here.
		panic("qbft: BuildExtra encode failed: " + err.Error())
	}
	return extra
}

// --- consensus.BFTEngine methods ---

// Quorum returns the minimum number of validator signatures required to commit
// a QBFT block: floor(2N/3) + 1.
func (e *Engine) Quorum(validatorCount int) int {
	return (2*validatorCount)/3 + 1
}

// VerifyProposal validates a block received in the PROPOSAL phase.
// Unlike VerifyHeader it does not check committed seals (they do not exist yet).
func (e *Engine) VerifyProposal(snap consensus.Snapshot, header *types.Header, parent *types.Header) error {
	number := header.Number.Uint64()

	if header.UncleHash != emptyUncleHash {
		return ErrInvalidUncleHash
	}
	if header.Difficulty == nil || header.Difficulty.Cmp(big.NewInt(1)) != 0 {
		return ErrInvalidDifficulty
	}
	if parent != nil {
		minTime := parent.Time + e.period
		if header.Time < minTime {
			return fmt.Errorf("%w: have %d, need >= %d", ErrInvalidTimestamp, header.Time, minTime)
		}
	}
	if len(header.Extra) < ExtraVanity {
		return fmt.Errorf("%w: too short at block %d", ErrInvalidExtraData, number)
	}
	if _, err := DecodeExtra(header); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidExtraData, err)
	}

	proposer, err := SignerFromHeader(header)
	if err != nil {
		return fmt.Errorf("qbft: recover proposer: %w", err)
	}
	if !snap.IsAuthorized(proposer) {
		return fmt.Errorf("%w: %s", ErrUnauthorizedValidator, proposer)
	}
	return nil
}

// CommitBlock injects the collected committed seals into header's Extra and
// returns the new final header.
func (e *Engine) CommitBlock(header *types.Header, committedSeals [][]byte) (*types.Header, error) {
	ie, err := DecodeExtra(header)
	if err != nil {
		return nil, err
	}
	ie.CommittedSeals = committedSeals
	extra, err := EncodeExtra(header.Extra[:ExtraVanity], ie)
	if err != nil {
		return nil, err
	}
	// Copy the header to avoid mutating the caller's value.
	committed := *header
	committed.Extra = extra
	return &committed, nil
}

// RequestTimeout returns the QBFT round timeout duration.
func (e *Engine) RequestTimeout() time.Duration { return e.requestTimeout }

// CreateCommitSeal signs the commit seal hash for header and returns the 65-byte seal.
func (e *Engine) CreateCommitSeal(header *types.Header, key *ecdsa.PrivateKey) ([]byte, error) {
	return CreateCommitSeal(header, key)
}

// RecoverCommitSealSigner recovers the validator address from a commit seal.
func (e *Engine) RecoverCommitSealSigner(header *types.Header, seal []byte) (common.Address, error) {
	return RecoverCommitSealSigner(header, seal)
}
