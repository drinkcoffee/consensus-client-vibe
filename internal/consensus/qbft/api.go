package qbft

import "github.com/peterrobinson/consensus-client-vibe/internal/consensus"

// Compile-time interface assertions.
var _ consensus.Engine    = (*Engine)(nil)
var _ consensus.Snapshot  = (*Snapshot)(nil)
var _ consensus.BFTEngine = (*Engine)(nil)
