package clique

import (
	"crypto/ecdsa"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// testKey generates a fresh ECDSA key for use in a single test.
func testKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

// addr returns the Ethereum address for the given private key.
func addr(key *ecdsa.PrivateKey) common.Address {
	return crypto.PubkeyToAddress(key.PublicKey)
}

// genesisExtra builds a genesis extra-data field containing the given signers
// (sorted by address) between the 32-byte vanity prefix and 65-byte seal suffix.
func genesisExtra(signers []common.Address) []byte {
	extra := make([]byte, extraVanity+len(signers)*common.AddressLength+extraSeal)
	for i, s := range signers {
		copy(extra[extraVanity+i*common.AddressLength:], s[:])
	}
	return extra
}

// epochExtra builds an extra-data field for an epoch checkpoint block.
func epochExtra(signers []common.Address) []byte {
	return genesisExtra(signers) // same layout
}

// plainExtra returns a minimal extra-data with just vanity + zeroed seal.
func plainExtra() []byte {
	return make([]byte, extraVanity+extraSeal)
}

// makeHeader builds a complete, sealed Clique header.
//
//   - number is the block number.
//   - parent is the parent header (may be nil for genesis-adjacent blocks).
//   - key is the private key used to seal the header.
//   - snap is used to determine correct difficulty.
//   - inTurnOverride forces the difficulty to in-turn (2) if true, else out-of-turn (1).
//     Pass a non-nil snap and set inTurnOverride to false to auto-detect.
//   - extra is the pre-built extra-data slice (seal region must be zeroed).
//   - vote, if non-nil, sets the coinbase and nonce for a vote.
func makeHeader(
	t *testing.T,
	number uint64,
	parent *types.Header,
	key *ecdsa.PrivateKey,
	snap *Snapshot,
	extra []byte,
	vote *voteOpt,
) *types.Header {
	t.Helper()

	signer := addr(key)

	var parentHash common.Hash
	var parentTime uint64
	if parent != nil {
		parentHash = parent.Hash()
		parentTime = parent.Time
	}

	// Difficulty: auto-detect from snapshot if provided.
	diff := big.NewInt(diffNoTurn)
	if snap != nil && snap.InTurn(number, signer) {
		diff = big.NewInt(diffInTurn)
	}

	nonce := nonceNull
	coinbase := common.Address{}
	if vote != nil {
		coinbase = vote.address
		if vote.authorize {
			nonce = nonceAuth
		}
	}

	h := &types.Header{
		Number:     new(big.Int).SetUint64(number),
		ParentHash: parentHash,
		UncleHash:  types.CalcUncleHash(nil),
		Difficulty: diff,
		Extra:      extra,
		Nonce:      nonce,
		Coinbase:   coinbase,
		Time:       parentTime + 15,
		MixDigest:  common.Hash{},
	}

	if err := SealHeader(h, key); err != nil {
		t.Fatalf("SealHeader: %v", err)
	}
	return h
}

// voteOpt carries the parameters for embedding a vote in a block header.
type voteOpt struct {
	address   common.Address
	authorize bool // true = add, false = remove
}

// makeSnap builds a genesis snapshot from the given sorted signer addresses.
func makeSnap(signers []common.Address) *Snapshot {
	genesis := &types.Header{
		Number: big.NewInt(0),
		Extra:  genesisExtra(signers),
	}
	snap, err := NewGenesisSnapshot(genesis)
	if err != nil {
		panic("makeSnap: " + err.Error())
	}
	return snap
}
