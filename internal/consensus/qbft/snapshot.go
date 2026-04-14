package qbft

import (
	"fmt"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/peterrobinson/consensus-client-vibe/internal/consensus"
)

// Snapshot holds the validator-authorization state at a specific block.
type Snapshot struct {
	Number     uint64
	Hash       common.Hash
	Validators map[common.Address]struct{}
}

// newSnapshot creates a Snapshot from a set of validators.
func newSnapshot(number uint64, hash common.Hash, validators []common.Address) *Snapshot {
	s := &Snapshot{
		Number:     number,
		Hash:       hash,
		Validators: make(map[common.Address]struct{}, len(validators)),
	}
	for _, v := range validators {
		s.Validators[v] = struct{}{}
	}
	return s
}

// --- consensus.Snapshot implementation ---

// BlockNumber returns the block number at which this snapshot was taken.
func (s *Snapshot) BlockNumber() uint64 { return s.Number }

// BlockHash returns the block hash at which this snapshot was taken.
func (s *Snapshot) BlockHash() common.Hash { return s.Hash }

// IsAuthorized reports whether addr is in the current validator set.
func (s *Snapshot) IsAuthorized(addr common.Address) bool {
	_, ok := s.Validators[addr]
	return ok
}

// HasRecentlySigned always returns false for QBFT. QBFT does not enforce a
// per-validator cooldown at the snapshot level; the BFT protocol prevents
// double proposals.
func (s *Snapshot) HasRecentlySigned(_ uint64, _ common.Address) bool {
	return false
}

// SignerList returns all validators sorted lexicographically by address.
func (s *Snapshot) SignerList() []common.Address {
	list := make([]common.Address, 0, len(s.Validators))
	for addr := range s.Validators {
		list = append(list, addr)
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].Hex() < list[j].Hex()
	})
	return list
}

// InTurn reports whether signer is the designated proposer for the given block
// number at round 0. The proposer is validators[number % N], sorted by address.
func (s *Snapshot) InTurn(number uint64, signer common.Address) bool {
	validators := s.SignerList()
	if len(validators) == 0 {
		return false
	}
	idx := int(number % uint64(len(validators)))
	return validators[idx] == signer
}

// PendingVotes always returns nil. QBFT does not use header-embedded votes;
// validator set changes happen at epoch boundaries via IstanbulExtra.Validators.
func (s *Snapshot) PendingVotes() []consensus.VoteRecord { return nil }

// apply creates a new Snapshot by processing a sequence of consecutive headers.
// At epoch boundaries, the validator set is read from IstanbulExtra.Validators.
func (s *Snapshot) apply(headers []*types.Header, epoch uint64) (*Snapshot, error) {
	if len(headers) == 0 {
		return s, nil
	}
	next := &Snapshot{
		Number:     s.Number,
		Hash:       s.Hash,
		Validators: make(map[common.Address]struct{}, len(s.Validators)),
	}
	for k, v := range s.Validators {
		next.Validators[k] = v
	}

	for _, h := range headers {
		num := h.Number.Uint64()
		if num%epoch == 0 {
			ie, err := DecodeExtra(h)
			if err != nil {
				return nil, fmt.Errorf("qbft snapshot apply: block %d: %w", num, err)
			}
			if len(ie.Validators) == 0 {
				return nil, fmt.Errorf("qbft snapshot apply: block %d epoch checkpoint has empty validator list", num)
			}
			next.Validators = make(map[common.Address]struct{}, len(ie.Validators))
			for _, v := range ie.Validators {
				next.Validators[v] = struct{}{}
			}
		}
		next.Number = num
		next.Hash = h.Hash()
	}
	return next, nil
}
