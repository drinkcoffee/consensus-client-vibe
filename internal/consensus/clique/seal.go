package clique

import (
	"crypto/ecdsa"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
)

// sigHash computes the hash over the header fields covered by the signer's ECDSA
// signature. It is the keccak256 of the RLP-encoded header with the 65-byte seal
// stripped from the end of Extra.
//
// Standard pre-EIP-1559 fields are always included. BaseFee is included when
// non-nil (EIP-1559 networks). Post-merge fields (WithdrawalsHash, BlobGasUsed,
// etc.) are intentionally omitted — Clique networks should not activate those forks.
func sigHash(header *types.Header) common.Hash {
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
		header.Extra[:len(header.Extra)-extraSeal], // strip seal
		header.MixDigest,
		header.Nonce,
	}
	// Include BaseFee for EIP-1559-enabled Clique networks.
	if header.BaseFee != nil {
		enc = append(enc, header.BaseFee)
	}

	encoded, err := rlp.EncodeToBytes(enc)
	if err != nil {
		// This can only happen due to a programming error (e.g. non-RLP-encodable
		// type in enc). Panic rather than silently produce a wrong hash.
		panic("clique: failed to RLP-encode header for sigHash: " + err.Error())
	}
	return crypto.Keccak256Hash(encoded)
}

// SignerFromHeader recovers the Ethereum address of the Clique signer from a
// sealed block header. The signature is the last extraSeal (65) bytes of
// header.Extra.
func SignerFromHeader(header *types.Header) (common.Address, error) {
	if len(header.Extra) < extraSeal {
		return common.Address{}, fmt.Errorf(
			"extra data too short: have %d bytes, need at least %d for seal",
			len(header.Extra), extraSeal)
	}

	sig := header.Extra[len(header.Extra)-extraSeal:]
	hash := sigHash(header)

	pubkey, err := crypto.Ecrecover(hash.Bytes(), sig)
	if err != nil {
		return common.Address{}, fmt.Errorf("ecrecover: %w", err)
	}
	if len(pubkey) == 0 || pubkey[0] != 4 {
		return common.Address{}, fmt.Errorf("invalid uncompressed public key recovered")
	}

	var signer common.Address
	copy(signer[:], crypto.Keccak256(pubkey[1:])[12:])
	return signer, nil
}

// SealHeader signs the header with key and writes the resulting 65-byte ECDSA
// signature into the last extraSeal bytes of header.Extra.
//
// header.Extra must be pre-allocated with at least extraVanity+extraSeal bytes
// and the seal region (last extraSeal bytes) must be zeroed before calling.
func SealHeader(header *types.Header, key *ecdsa.PrivateKey) error {
	if len(header.Extra) < extraVanity+extraSeal {
		return fmt.Errorf(
			"extra data too short to seal: have %d bytes, need at least %d",
			len(header.Extra), extraVanity+extraSeal)
	}

	hash := sigHash(header)
	sig, err := crypto.Sign(hash.Bytes(), key)
	if err != nil {
		return fmt.Errorf("sign header hash: %w", err)
	}

	copy(header.Extra[len(header.Extra)-extraSeal:], sig)
	return nil
}
