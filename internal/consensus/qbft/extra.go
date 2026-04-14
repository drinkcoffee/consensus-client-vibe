// Package qbft implements the QBFT (Quorum Byzantine Fault Tolerance)
// proof-of-authority consensus mechanism. QBFT is a three-phase BFT protocol
// that requires 2f+1 validator agreement before a block is considered final.
package qbft

import (
	"crypto/ecdsa"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
)

const (
	// ExtraVanity is the fixed byte count of the vanity prefix in Extra.
	ExtraVanity = 32
	// ExtraSeal is the fixed byte count of the proposer ECDSA seal in Extra.
	// Located immediately after the vanity bytes, before the RLP(IstanbulExtra).
	ExtraSeal = 65
)

// IstanbulExtra is the QBFT-specific data embedded in the block header Extra
// field after the 32-byte vanity prefix.
//
// Wire format: Extra = [32 vanity bytes | RLP(IstanbulExtra)]
type IstanbulExtra struct {
	// Validators is the new validator set encoded at epoch boundaries; nil otherwise.
	Validators []common.Address
	// Vote is an optional RLP-encoded validator vote; nil if no vote.
	Vote []byte
	// Round is the QBFT round number in which this block was proposed.
	Round uint32
	// Seal is the 65-byte ECDSA proposer signature over the proposal hash.
	// Nil in an unsealed proposal header; set by SealHeader.
	Seal []byte
	// CommittedSeals are the 65-byte ECDSA commit seals from 2f+1 validators.
	// Nil in proposal headers; injected by CommitBlock once quorum is reached.
	CommittedSeals [][]byte
}

// EncodeExtra serialises vanity and ie into the wire-format Extra bytes.
// vanity must be exactly ExtraVanity bytes; it is zero-padded if shorter.
func EncodeExtra(vanity []byte, ie *IstanbulExtra) ([]byte, error) {
	pad := make([]byte, ExtraVanity)
	copy(pad, vanity)

	enc, err := rlp.EncodeToBytes(ie)
	if err != nil {
		return nil, fmt.Errorf("qbft: RLP encode IstanbulExtra: %w", err)
	}
	return append(pad, enc...), nil
}

// DecodeExtra parses the QBFT Extra field from a header.
// Returns an error if the Extra is too short or the RLP is invalid.
func DecodeExtra(header *types.Header) (*IstanbulExtra, error) {
	if len(header.Extra) < ExtraVanity {
		return nil, fmt.Errorf("qbft: extra too short: %d bytes (need ≥ %d)", len(header.Extra), ExtraVanity)
	}
	var ie IstanbulExtra
	if err := rlp.DecodeBytes(header.Extra[ExtraVanity:], &ie); err != nil {
		return nil, fmt.Errorf("qbft: decode IstanbulExtra: %w", err)
	}
	return &ie, nil
}

// proposalSigHash returns the hash that the proposer signs when sealing a block.
// It uses the full header field list (like Clique's sigHash) but with both
// Seal and CommittedSeals set to nil so they are excluded from the signed data.
func proposalSigHash(header *types.Header) (common.Hash, error) {
	// Build a stripped Extra: vanity only, with Seal=nil, CommittedSeals=nil.
	ie, err := DecodeExtra(header)
	if err != nil {
		return common.Hash{}, err
	}
	stripped := IstanbulExtra{
		Validators:     ie.Validators,
		Vote:           ie.Vote,
		Round:          ie.Round,
		Seal:           nil,
		CommittedSeals: nil,
	}
	strippedExtra, err := EncodeExtra(header.Extra[:ExtraVanity], &stripped)
	if err != nil {
		return common.Hash{}, err
	}

	enc := []interface{}{
		header.ParentHash,
		header.UncleHash,
		header.Coinbase,
		header.Root,
		header.TxHash,
		header.ReceiptHash,
		header.Bloom,
		header.Difficulty,
		header.Number,
		header.GasLimit,
		header.GasUsed,
		header.Time,
		strippedExtra,
		header.MixDigest,
		header.Nonce,
	}
	if header.BaseFee != nil {
		enc = append(enc, header.BaseFee)
	}
	encoded, err := rlp.EncodeToBytes(enc)
	if err != nil {
		return common.Hash{}, fmt.Errorf("qbft: RLP encode for proposalSigHash: %w", err)
	}
	return crypto.Keccak256Hash(encoded), nil
}

// commitSealHash returns the hash that validators sign when committing a block.
// It is keccak256(0x01 ++ header.Hash()) where header has Seal set but
// CommittedSeals=nil. The 0x01 prefix prevents replay of proposal seals.
func commitSealHash(header *types.Header) (common.Hash, error) {
	// Build header with CommittedSeals stripped.
	ie, err := DecodeExtra(header)
	if err != nil {
		return common.Hash{}, err
	}
	stripped := IstanbulExtra{
		Validators:     ie.Validators,
		Vote:           ie.Vote,
		Round:          ie.Round,
		Seal:           ie.Seal,
		CommittedSeals: nil,
	}
	strippedExtra, err := EncodeExtra(header.Extra[:ExtraVanity], &stripped)
	if err != nil {
		return common.Hash{}, err
	}

	// Temporarily substitute the stripped Extra to compute the header hash.
	origExtra := header.Extra
	header.Extra = strippedExtra
	h := header.Hash()
	header.Extra = origExtra // restore

	prefix := []byte{0x01}
	return crypto.Keccak256Hash(append(prefix, h.Bytes()...)), nil
}

// SealHeader writes the 65-byte ECDSA proposer signature into header's
// IstanbulExtra.Seal field.
func SealHeader(header *types.Header, key *ecdsa.PrivateKey) error {
	hash, err := proposalSigHash(header)
	if err != nil {
		return err
	}
	sig, err := crypto.Sign(hash.Bytes(), key)
	if err != nil {
		return fmt.Errorf("qbft: sign proposal hash: %w", err)
	}

	ie, err := DecodeExtra(header)
	if err != nil {
		return err
	}
	ie.Seal = sig
	extra, err := EncodeExtra(header.Extra[:ExtraVanity], ie)
	if err != nil {
		return err
	}
	header.Extra = extra
	return nil
}

// SignerFromHeader recovers the proposer address from a sealed QBFT header.
func SignerFromHeader(header *types.Header) (common.Address, error) {
	ie, err := DecodeExtra(header)
	if err != nil {
		return common.Address{}, err
	}
	if len(ie.Seal) != ExtraSeal {
		return common.Address{}, fmt.Errorf("qbft: proposer seal has wrong length %d", len(ie.Seal))
	}
	hash, err := proposalSigHash(header)
	if err != nil {
		return common.Address{}, err
	}
	pubkey, err := crypto.Ecrecover(hash.Bytes(), ie.Seal)
	if err != nil {
		return common.Address{}, fmt.Errorf("qbft: ecrecover proposer: %w", err)
	}
	if len(pubkey) == 0 || pubkey[0] != 4 {
		return common.Address{}, fmt.Errorf("qbft: invalid uncompressed public key")
	}
	var addr common.Address
	copy(addr[:], crypto.Keccak256(pubkey[1:])[12:])
	return addr, nil
}

// CreateCommitSeal signs the commit seal hash for header and returns the 65-byte seal.
func CreateCommitSeal(header *types.Header, key *ecdsa.PrivateKey) ([]byte, error) {
	hash, err := commitSealHash(header)
	if err != nil {
		return nil, err
	}
	sig, err := crypto.Sign(hash.Bytes(), key)
	if err != nil {
		return nil, fmt.Errorf("qbft: sign commit hash: %w", err)
	}
	return sig, nil
}

// RecoverCommitSealSigner recovers the validator address from a commit seal.
func RecoverCommitSealSigner(header *types.Header, seal []byte) (common.Address, error) {
	hash, err := commitSealHash(header)
	if err != nil {
		return common.Address{}, err
	}
	pubkey, err := crypto.Ecrecover(hash.Bytes(), seal)
	if err != nil {
		return common.Address{}, fmt.Errorf("qbft: ecrecover commit seal: %w", err)
	}
	if len(pubkey) == 0 || pubkey[0] != 4 {
		return common.Address{}, fmt.Errorf("qbft: invalid uncompressed public key in commit seal")
	}
	var addr common.Address
	copy(addr[:], crypto.Keccak256(pubkey[1:])[12:])
	return addr, nil
}
