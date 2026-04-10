// Package node wires all subsystems together and runs the Clique consensus
// client. It owns the block-processing pipeline (validate incoming P2P blocks,
// update fork choice, notify the execution client) and the block-production
// pipeline (detect our turn, build a payload, seal the Clique header, broadcast
// and import).
package node

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	gethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	libp2ppeer "github.com/libp2p/go-libp2p/core/peer"
	"github.com/rs/zerolog"

	cliqueeng "github.com/peterrobinson/consensus-client-vibe/internal/clique"
	"github.com/peterrobinson/consensus-client-vibe/internal/config"
	"github.com/peterrobinson/consensus-client-vibe/internal/engine"
	"github.com/peterrobinson/consensus-client-vibe/internal/forkchoice"
	"github.com/peterrobinson/consensus-client-vibe/internal/log"
	p2phost "github.com/peterrobinson/consensus-client-vibe/internal/p2p"
	"github.com/peterrobinson/consensus-client-vibe/internal/rpc"
)

// EngineAPI is the subset of *engine.Client methods used by the node.
// It is defined as an interface so that tests can substitute a mock.
type EngineAPI interface {
	ExchangeCapabilities(ctx context.Context) ([]string, error)
	NewPayloadV3(ctx context.Context, payload engine.ExecutionPayloadV3, versionedHashes []common.Hash, parentBeaconRoot common.Hash) (engine.PayloadStatusV1, error)
	ForkchoiceUpdatedV3(ctx context.Context, state engine.ForkchoiceStateV1, attrs *engine.PayloadAttributesV3) (engine.ForkchoiceUpdatedResult, error)
	GetPayloadV3(ctx context.Context, id engine.PayloadID) (engine.GetPayloadResponseV3, error)
}

// Node is the top-level orchestrator. It holds all subsystems and runs the
// main event loop.
type Node struct {
	cfg  *config.Config
	eng  EngineAPI
	p2p  *p2phost.Host
	stor *forkchoice.Store
	cliq *cliqueeng.Engine
	rpc  *rpc.Server

	signerKey  *ecdsa.PrivateKey // nil in follower mode
	signerAddr common.Address   // zero in follower mode

	// Clique snapshot cache. genesisSnap is the baseline. headSnap is the
	// snapshot at the current canonical head. epochSnaps caches epoch-
	// boundary snapshots to avoid replaying from genesis on every reorg.
	mu         sync.RWMutex
	genesisSnap *cliqueeng.Snapshot
	headSnap    *cliqueeng.Snapshot
	epochSnaps  map[uint64]*cliqueeng.Snapshot // epoch start block number → snapshot

	// Block production timer.
	prodMu    sync.Mutex
	prodTimer *timerCancel

	log zerolog.Logger
}

// timerCancel wraps a time.Timer and a cancel function so that an in-flight
// produceBlock call can be aborted when the timer is replaced.
type timerCancel struct {
	cancel context.CancelFunc
}

// New creates a Node and initialises all subsystems. It connects to the
// execution client to fetch the genesis block, then wires the Engine API
// client, P2P host, fork-choice store, Clique engine, and RPC server.
func New(cfg *config.Config) (*Node, error) {
	logger := log.With("node")

	// --- Signer key ---
	var signerKey *ecdsa.PrivateKey
	var signerAddr common.Address
	if cfg.Clique.SignerKeyPath != "" {
		k, err := loadSignerKey(cfg.Clique.SignerKeyPath)
		if err != nil {
			return nil, fmt.Errorf("load signer key: %w", err)
		}
		signerKey = k
		signerAddr = gethcrypto.PubkeyToAddress(k.PublicKey)
		logger.Info().Str("address", signerAddr.Hex()).Msg("signer key loaded")
	} else {
		logger.Info().Msg("no signer key configured — running in follower mode")
	}

	// --- Engine API client ---
	jwt, err := engine.NewJWTProvider(cfg.Engine.JWTSecretPath)
	if err != nil {
		return nil, fmt.Errorf("create JWT provider: %w", err)
	}
	engClient := engine.New(cfg.Engine.URL, jwt, cfg.Engine.CallTimeout.Duration)

	// --- Genesis block (fetched from EL via regular JSON-RPC) ---
	elClient, err := ethclient.Dial(cfg.Engine.ELRPCUrl)
	if err != nil {
		return nil, fmt.Errorf("connect to EL JSON-RPC (%s): %w", cfg.Engine.ELRPCUrl, err)
	}
	defer elClient.Close()

	ctx := context.Background()
	genesisBlock, err := elClient.BlockByNumber(ctx, big.NewInt(0))
	if err != nil {
		return nil, fmt.Errorf("fetch genesis block from EL: %w", err)
	}
	genesisHeader := genesisBlock.Header()
	logger.Info().
		Str("hash", genesisHeader.Hash().Hex()).
		Msg("genesis block loaded from EL")

	// --- Genesis snapshot ---
	genesisSnap, err := cliqueeng.NewGenesisSnapshot(genesisHeader)
	if err != nil {
		return nil, fmt.Errorf("init genesis snapshot: %w", err)
	}

	// --- Subsystems ---
	stor := forkchoice.New(genesisHeader, cfg.Clique.Epoch)
	cliq := cliqueeng.New(cfg.Clique.Period, cfg.Clique.Epoch)

	genesisHash := genesisHeader.Hash()
	p2pH, err := p2phost.New(&cfg.P2P, p2phost.StatusMsg{
		NetworkID:   cfg.Node.NetworkID,
		GenesisHash: genesisHash,
		HeadHash:    genesisHash,
		HeadNumber:  0,
	})
	if err != nil {
		return nil, fmt.Errorf("create P2P host: %w", err)
	}

	n := &Node{
		cfg:         cfg,
		eng:         engClient,
		p2p:         p2pH,
		stor:        stor,
		cliq:        cliq,
		signerKey:   signerKey,
		signerAddr:  signerAddr,
		genesisSnap: genesisSnap,
		headSnap:    genesisSnap,
		epochSnaps:  make(map[uint64]*cliqueeng.Snapshot),
		log:         logger,
	}

	// RPC server wired to live node state via closures.
	n.rpc = rpc.New(&cfg.RPC, p2pH, stor, func() *cliqueeng.Snapshot {
		n.mu.RLock()
		defer n.mu.RUnlock()
		return n.headSnap
	})

	return n, nil
}

// Start runs the node until ctx is cancelled. It:
//  1. Handshakes with the execution client (engine_exchangeCapabilities).
//  2. Starts P2P networking and the RPC server.
//  3. Schedules the first block-production slot.
//  4. Blocks until ctx is cancelled, then shuts down cleanly.
func (n *Node) Start(ctx context.Context) error {
	// 1. Engine API handshake.
	caps, err := n.eng.ExchangeCapabilities(ctx)
	if err != nil {
		return fmt.Errorf("engine API handshake: %w", err)
	}
	n.log.Info().Strs("capabilities", caps).Msg("engine API handshake OK")

	// 2. Register the P2P block handler and start the host.
	n.p2p.SetBlockHandler(func(_ libp2ppeer.ID, blk *p2phost.CliqueBlock) {
		n.handleBlock(ctx, blk)
	})
	if err := n.p2p.Start(ctx, &n.cfg.P2P); err != nil {
		return fmt.Errorf("start P2P: %w", err)
	}

	// 3. Start RPC server in background.
	go func() {
		if err := n.rpc.Start(); err != nil {
			n.log.Error().Err(err).Msg("RPC server error")
		}
	}()

	// 4. Schedule first production slot.
	n.scheduleBlockProduction(ctx)

	n.log.Info().Msg("node started")

	// 5. Wait for shutdown.
	<-ctx.Done()
	return n.shutdown()
}

// shutdown performs a graceful teardown of all subsystems.
func (n *Node) shutdown() error {
	n.log.Info().Msg("shutting down")

	// Stop production timer.
	n.prodMu.Lock()
	if n.prodTimer != nil {
		n.prodTimer.cancel()
		n.prodTimer = nil
	}
	n.prodMu.Unlock()

	// Stop RPC server.
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := n.rpc.Stop(shutCtx); err != nil {
		n.log.Warn().Err(err).Msg("RPC server shutdown error")
	}

	// Close P2P host.
	if err := n.p2p.Close(); err != nil {
		n.log.Warn().Err(err).Msg("P2P close error")
	}

	n.log.Info().Msg("shutdown complete")
	return nil
}

// --- Snapshot management ---

// headSnapshot returns the Clique snapshot at the current canonical head.
func (n *Node) headSnapshot() *cliqueeng.Snapshot {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.headSnap
}

// setHeadSnapshot updates the head snapshot and caches it at epoch boundaries.
func (n *Node) setHeadSnapshot(snap *cliqueeng.Snapshot) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.headSnap = snap
	// Cache epoch-boundary snapshots for efficient reorg recovery.
	if snap.Number%n.cfg.Clique.Epoch == 0 {
		n.epochSnaps[snap.Number] = snap
	}
}

// computeSnapshot returns the Clique snapshot at the given header by
// advancing from the nearest cached epoch checkpoint.
func (n *Node) computeSnapshot(header *types.Header) (*cliqueeng.Snapshot, error) {
	num := header.Number.Uint64()

	// Fast path: extending the current head by one block.
	n.mu.RLock()
	cur := n.headSnap
	n.mu.RUnlock()
	if cur != nil && cur.Number+1 == num && cur.Hash == header.ParentHash {
		return n.cliq.Apply(cur, []*types.Header{header})
	}

	// General path: start from the nearest epoch checkpoint ≤ num.
	epochStart := (num / n.cfg.Clique.Epoch) * n.cfg.Clique.Epoch

	n.mu.RLock()
	base, ok := n.epochSnaps[epochStart]
	n.mu.RUnlock()

	if !ok {
		if epochStart == 0 {
			base = n.genesisSnap
		} else {
			epochHeader, exists := n.stor.GetByNumber(epochStart)
			if !exists {
				return nil, fmt.Errorf("epoch header at %d not in store", epochStart)
			}
			var err error
			base, err = cliqueeng.NewCheckpointSnapshot(epochHeader)
			if err != nil {
				return nil, fmt.Errorf("build epoch snapshot at %d: %w", epochStart, err)
			}
			n.mu.Lock()
			n.epochSnaps[epochStart] = base
			n.mu.Unlock()
		}
	}

	// Collect headers from epochStart+1 to num.
	headers := make([]*types.Header, 0, num-epochStart)
	for i := epochStart + 1; i <= num; i++ {
		h, exists := n.stor.GetByNumber(i)
		if !exists {
			return nil, fmt.Errorf("block %d not in store (needed for snapshot)", i)
		}
		headers = append(headers, h)
	}
	if len(headers) == 0 {
		return base, nil
	}
	return n.cliq.Apply(base, headers)
}

// --- helpers ---

// loadSignerKey reads a hex-encoded ECDSA private key from path.
func loadSignerKey(path string) (*ecdsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	hex := strings.TrimSpace(string(data))
	hex = strings.TrimPrefix(hex, "0x")
	return gethcrypto.HexToECDSA(hex)
}
