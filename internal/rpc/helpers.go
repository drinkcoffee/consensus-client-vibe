package rpc

import (
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

// zeroAddress returns the all-zero Ethereum address.
func zeroAddress() common.Address {
	return common.Address{}
}

// isHexAddress returns true if s is a valid 0x-prefixed 20-byte hex address.
func isHexAddress(s string) bool {
	return common.IsHexAddress(s)
}

// hexToAddress converts a hex string to a common.Address.
func hexToAddress(s string) common.Address {
	return common.HexToAddress(s)
}

// nonZeroHexAddress reports whether the hex-encoded address is non-zero.
// It trims the "0x" prefix before comparison.
func nonZeroHexAddress(s string) bool {
	s = strings.TrimPrefix(strings.ToLower(s), "0x")
	for _, c := range s {
		if c != '0' {
			return true
		}
	}
	return false
}
