package clique

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// Snapshot is the state of the Clique authorization vote at a given block.
// It records which signers are currently authorized, which signers have signed
// recently (for the "must wait" rule), and any pending votes.
type Snapshot struct {
	// Number is the block number at which this snapshot was taken.
	Number uint64
	// Hash is the block hash at which this snapshot was taken.
	Hash common.Hash
	// Signers is the set of currently authorized block signers.
	Signers map[common.Address]struct{}
	// Recents maps recent block numbers to the signer who signed them.
	// Used to enforce EIP-225 §3: a signer cannot sign within
	// floor(len(signers)/2)+1 of their last signing block.
	Recents map[uint64]common.Address
	// Votes is the ordered list of all pending votes cast in the current epoch.
	Votes []*Vote
	// Tally is the current vote count per candidate address.
	Tally map[common.Address]Tally
}

// newSnapshot creates a bare snapshot with the given signers and no votes.
func newSnapshot(number uint64, hash common.Hash, signers []common.Address) *Snapshot {
	s := &Snapshot{
		Number:  number,
		Hash:    hash,
		Signers: make(map[common.Address]struct{}, len(signers)),
		Recents: make(map[uint64]common.Address),
		Tally:   make(map[common.Address]Tally),
	}
	for _, addr := range signers {
		s.Signers[addr] = struct{}{}
	}
	return s
}

// NewGenesisSnapshot derives the initial Clique snapshot from the genesis block
// header. The genesis extra data must contain the initial signer list between
// the 32-byte vanity prefix and the 65-byte seal suffix.
func NewGenesisSnapshot(genesis *types.Header) (*Snapshot, error) {
	extra := genesis.Extra
	if len(extra) < extraVanity+extraSeal {
		return nil, fmt.Errorf("genesis extra data too short: have %d bytes, need %d",
			len(extra), extraVanity+extraSeal)
	}
	signerData := extra[extraVanity : len(extra)-extraSeal]
	if len(signerData) == 0 {
		return nil, errors.New("genesis extra data contains no signers")
	}
	if len(signerData)%common.AddressLength != 0 {
		return nil, fmt.Errorf("genesis signer data length %d is not a multiple of %d",
			len(signerData), common.AddressLength)
	}

	count := len(signerData) / common.AddressLength
	signers := make([]common.Address, count)
	for i := range signers {
		copy(signers[i][:], signerData[i*common.AddressLength:])
	}

	return newSnapshot(genesis.Number.Uint64(), genesis.Hash(), signers), nil
}

// copy returns a deep copy of s so that apply can be called without mutating s.
func (s *Snapshot) copy() *Snapshot {
	cpy := &Snapshot{
		Number:  s.Number,
		Hash:    s.Hash,
		Signers: make(map[common.Address]struct{}, len(s.Signers)),
		Recents: make(map[uint64]common.Address, len(s.Recents)),
		Tally:   make(map[common.Address]Tally, len(s.Tally)),
	}
	for addr := range s.Signers {
		cpy.Signers[addr] = struct{}{}
	}
	for block, addr := range s.Recents {
		cpy.Recents[block] = addr
	}
	if s.Votes != nil {
		cpy.Votes = make([]*Vote, len(s.Votes))
		for i, v := range s.Votes {
			vc := *v
			cpy.Votes[i] = &vc
		}
	}
	for addr, t := range s.Tally {
		cpy.Tally[addr] = t
	}
	return cpy
}

// SignerList returns the current authorized signers sorted lexicographically by
// address. This ordering determines in-turn assignment.
func (s *Snapshot) SignerList() []common.Address {
	list := make([]common.Address, 0, len(s.Signers))
	for addr := range s.Signers {
		list = append(list, addr)
	}
	sort.Slice(list, func(i, j int) bool {
		return bytes.Compare(list[i][:], list[j][:]) < 0
	})
	return list
}

// IsAuthorized reports whether addr is an authorized signer.
func (s *Snapshot) IsAuthorized(addr common.Address) bool {
	_, ok := s.Signers[addr]
	return ok
}

// InTurn reports whether signer is the designated in-turn signer for the given
// block number (i.e. its position in the sorted signer list equals number mod N).
func (s *Snapshot) InTurn(number uint64, signer common.Address) bool {
	list := s.SignerList()
	for i, addr := range list {
		if addr == signer {
			return uint64(i) == number%uint64(len(list))
		}
	}
	return false
}

// HasRecentlySigned reports whether signer has signed too recently to be
// permitted to sign the block at number. EIP-225 §3 requires that a signer
// waits at least floor(N/2)+1 blocks between signings.
func (s *Snapshot) HasRecentlySigned(number uint64, signer common.Address) bool {
	limit := uint64(len(s.Signers)/2 + 1)
	for blockNum, addr := range s.Recents {
		if addr == signer && blockNum+limit > number {
			return true
		}
	}
	return false
}

// validVote returns true if voting to authorize/deauthorize address makes sense
// given the current signer set: authorizing a non-signer or deauthorizing a signer.
func (s *Snapshot) validVote(address common.Address, authorize bool) bool {
	_, isSigner := s.Signers[address]
	return (authorize && !isSigner) || (!authorize && isSigner)
}

// cast records a vote for address in the given direction, incrementing the tally.
func (s *Snapshot) cast(address common.Address, authorize bool) {
	t, ok := s.Tally[address]
	if !ok || t.Authorize != authorize {
		s.Tally[address] = Tally{Authorize: authorize, Votes: 1}
		return
	}
	s.Tally[address] = Tally{Authorize: authorize, Votes: t.Votes + 1}
}

// uncast removes a previously recorded vote for address, decrementing the tally.
// No-ops if there is no matching tally entry.
func (s *Snapshot) uncast(address common.Address, authorize bool) {
	t, ok := s.Tally[address]
	if !ok || t.Authorize != authorize {
		return
	}
	if t.Votes > 1 {
		s.Tally[address] = Tally{Authorize: authorize, Votes: t.Votes - 1}
	} else {
		delete(s.Tally, address)
	}
}

// apply creates a new snapshot by replaying headers on top of s. The headers
// must be contiguous, starting at s.Number+1. epoch controls when vote resets
// and signer-list checkpoints occur.
func (s *Snapshot) apply(headers []*types.Header, epoch uint64) (*Snapshot, error) {
	if len(headers) == 0 {
		return s, nil
	}
	if headers[0].Number.Uint64() != s.Number+1 {
		return nil, fmt.Errorf("header %d does not follow snapshot at block %d",
			headers[0].Number, s.Number)
	}
	for i := 1; i < len(headers); i++ {
		if headers[i].Number.Uint64() != headers[i-1].Number.Uint64()+1 {
			return nil, fmt.Errorf("non-contiguous headers: %d → %d",
				headers[i-1].Number, headers[i].Number)
		}
	}

	snap := s.copy()

	for _, header := range headers {
		number := header.Number.Uint64()

		// At epoch boundaries: read the authoritative signer list from extra data
		// and discard all accumulated votes for this epoch.
		if number%epoch == 0 {
			signerData := header.Extra[extraVanity : len(header.Extra)-extraSeal]
			snap.Signers = make(map[common.Address]struct{}, len(signerData)/common.AddressLength)
			for i := 0; i < len(signerData); i += common.AddressLength {
				var addr common.Address
				copy(addr[:], signerData[i:])
				snap.Signers[addr] = struct{}{}
			}
			snap.Votes = nil
			snap.Tally = make(map[common.Address]Tally)
		}

		// Evict the oldest recent-signer entry, freeing that signer to sign again.
		limit := uint64(len(snap.Signers)/2 + 1)
		if number >= limit {
			delete(snap.Recents, number-limit)
		}

		// Recover and record the signer.
		signer, err := SignerFromHeader(header)
		if err != nil {
			return nil, fmt.Errorf("block %d: recover signer: %w", number, err)
		}
		if !snap.IsAuthorized(signer) {
			return nil, fmt.Errorf("block %d: %w", number, ErrUnauthorizedSigner)
		}
		for _, recent := range snap.Recents {
			if recent == signer {
				return nil, fmt.Errorf("block %d: %w", number, ErrRecentlySigned)
			}
		}
		snap.Recents[number] = signer

		// Process the vote embedded in this block (if any).
		// Votes are not processed at epoch boundaries (they have already been reset).
		nonce := binary.BigEndian.Uint64(header.Nonce[:])
		candidate := header.Coinbase
		isVote := candidate != (common.Address{}) &&
			(nonce == nonceAuthUint64 || nonce == nonceDropUint64) &&
			number%epoch != 0

		if isVote {
			authorize := nonce == nonceAuthUint64

			// Discard any previous vote by this signer on the same candidate so
			// that each (signer, candidate) pair counts at most once.
			for i, v := range snap.Votes {
				if v.Signer == signer && v.Address == candidate {
					snap.uncast(candidate, v.Authorize)
					snap.Votes = append(snap.Votes[:i], snap.Votes[i+1:]...)
					break
				}
			}

			// Only tally meaningful votes.
			if snap.validVote(candidate, authorize) {
				snap.Votes = append(snap.Votes, &Vote{
					Signer:    signer,
					Block:     number,
					Address:   candidate,
					Authorize: authorize,
				})
				snap.cast(candidate, authorize)

				// Check whether the vote has reached a simple majority.
				if snap.Tally[candidate].Votes > len(snap.Signers)/2 {
					if authorize {
						snap.Signers[candidate] = struct{}{}
					} else {
						delete(snap.Signers, candidate)
						// Remove the deauthorized signer from the recent list.
						for blk, addr := range snap.Recents {
							if addr == candidate {
								delete(snap.Recents, blk)
							}
						}
						// Discard all votes cast BY the removed signer.
						for i := 0; i < len(snap.Votes); i++ {
							if snap.Votes[i].Signer == candidate {
								snap.uncast(snap.Votes[i].Address, snap.Votes[i].Authorize)
								snap.Votes = append(snap.Votes[:i], snap.Votes[i+1:]...)
								i--
							}
						}
					}
					// Discard all votes FOR the candidate now that the vote is resolved.
					for i := 0; i < len(snap.Votes); i++ {
						if snap.Votes[i].Address == candidate {
							snap.Votes = append(snap.Votes[:i], snap.Votes[i+1:]...)
							i--
						}
					}
					delete(snap.Tally, candidate)
				}
			}
		}

		snap.Number = number
		snap.Hash = header.Hash()
	}

	return snap, nil
}
