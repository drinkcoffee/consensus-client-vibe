package clique

import "github.com/ethereum/go-ethereum/common"

// Vote represents a single vote cast by an authorized signer to add or remove
// another address from the signer set.
type Vote struct {
	// Signer is the authorized signer who cast this vote.
	Signer common.Address
	// Block is the block number at which this vote was cast.
	Block uint64
	// Address is the account being voted on (to be added or removed).
	Address common.Address
	// Authorize is true if this is a vote to add Address to the signer set,
	// or false if it is a vote to remove it.
	Authorize bool
}

// Tally tracks the current vote count for adding or removing a specific address.
// Only one direction (add or remove) can be active at a time; if the direction
// flips, the old count is discarded.
type Tally struct {
	// Authorize is the direction of the votes being counted.
	Authorize bool
	// Votes is the number of signers who have voted in this direction.
	Votes int
}
