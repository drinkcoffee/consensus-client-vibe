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
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	gethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	libp2ppeer "github.com/libp2p/go-libp2p/core/peer"
	"github.com/rs/zerolog"

	"github.com/peterrobinson/consensus-client-vibe/internal/consensus"
	cliqueeng "github.com/peterrobinson/consensus-client-vibe/internal/consensus/clique"
	qbfteng "github.com/peterrobinson/consensus-client-vibe/internal/consensus/qbft"
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
	cliq consensus.Engine
	rpc  *rpc.Server

	signerKey  *ecdsa.PrivateKey // nil in follower mode
	signerAddr common.Address   // zero in follower mode

	// genesisHeader is the genesis block header fetched from the EL at startup.
	// It is kept so the sync protocol can reset the store back to genesis.
	genesisHeader *types.Header

	// db is the on-disk chain journal. Nil when no data directory is configured.
	db *forkchoice.ChainDB

	// Consensus snapshot cache. genesisSnap is the baseline. headSnap is the
	// snapshot at the current canonical head. epochSnaps caches epoch-
	// boundary snapshots to avoid replaying from genesis on every reorg.
	mu          sync.RWMutex
	genesisSnap consensus.Snapshot
	headSnap    consensus.Snapshot
	epochSnaps  map[uint64]consensus.Snapshot // epoch start block number → snapshot

	// Block production timer (Clique only).
	prodMu    sync.Mutex
	prodTimer *timerCancel

	// qbftMsgCh receives incoming QBFT messages from the P2P handler and
	// forwards them to the active QBFT instance. Buffered to avoid blocking
	// the P2P goroutine.
	qbftMsgCh chan *p2phost.QBFTMsg

	// syncMu serialises sync sessions. handleBlock uses TryLock to yield when
	// a sync is actively rewriting the store.
	syncMu sync.Mutex

	// runCtx is the root lifecycle context set by Start. It is used as the
	// parent for all production contexts so that cancelling an in-flight
	// production slot (to replace the timer) does not also kill the next slot.
	runCtx context.Context

	log zerolog.Logger
}

// timerCancel wraps a time.Timer and a cancel function so that an in-flight
// produceBlock call can be aborted when the timer is replaced.
type timerCancel struct {
	cancel context.CancelFunc
}

// New creates a Node and initialises all subsystems. It connects to the
// execution client to fetch the genesis block, then wires the Engine API
// client, P2P host, fork-choice store, consensus engine, and RPC server.
func New(cfg *config.Config) (*Node, error) {
	logger := log.With("node")

	// --- Signer key ---
	// Both Clique and QBFT use an ECDSA key; which config field to read
	// depends on the consensus type.
	var signerKey *ecdsa.PrivateKey
	var signerAddr common.Address
	signerKeyPath := cfg.Consensus.Clique.SignerKeyPath
	if cfg.Consensus.Type == "qbft" {
		signerKeyPath = cfg.Consensus.QBFT.ValidatorKeyPath
	}
	if signerKeyPath != "" {
		k, err := loadSignerKey(signerKeyPath)
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

	// --- Consensus engine ---
	var cliq consensus.Engine
	switch cfg.Consensus.Type {
	case "", "clique":
		cliq = cliqueeng.New(cfg.Consensus.Clique.Period, cfg.Consensus.Clique.Epoch)
	case "qbft":
		cliq = qbfteng.New(
			cfg.Consensus.QBFT.Period,
			cfg.Consensus.QBFT.Epoch,
			time.Duration(cfg.Consensus.QBFT.RequestTimeoutMs)*time.Millisecond,
		)
	default:
		return nil, fmt.Errorf("unsupported consensus type %q", cfg.Consensus.Type)
	}

	// --- Genesis snapshot ---
	genesisSnap, err := cliq.NewGenesisSnapshot(genesisHeader)
	if err != nil {
		return nil, fmt.Errorf("init genesis snapshot: %w", err)
	}

	// --- Subsystems ---
	stor := forkchoice.New(genesisHeader, cliq.Epoch())

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
		cfg:           cfg,
		eng:           engClient,
		p2p:           p2pH,
		stor:          stor,
		cliq:          cliq,
		signerKey:     signerKey,
		signerAddr:    signerAddr,
		genesisHeader: genesisHeader,
		genesisSnap:   genesisSnap,
		headSnap:      genesisSnap,
		epochSnaps:    make(map[uint64]consensus.Snapshot),
		qbftMsgCh:     make(chan *p2phost.QBFTMsg, 64),
		log:           logger,
	}

	// --- Chain DB (optional persistence) ---
	if cfg.Node.DataDir != "" {
		dbPath := filepath.Join(cfg.Node.DataDir, "cl-headers.db")
		db, err := forkchoice.OpenChainDB(dbPath)
		if err != nil {
			logger.Warn().Err(err).Str("path", dbPath).Msg("failed to open chain DB, starting without persistence")
		} else {
			n.db = db
			n.replayChain(db)
		}
	}

	// RPC server wired to live node state via closures.
	n.rpc = rpc.New(&cfg.RPC, p2pH, stor, func() consensus.Snapshot {
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

	// 2. Register the P2P block and sync handlers, then start the host.
	n.p2p.SetBlockHandler(func(from libp2ppeer.ID, blk *p2phost.CliqueBlock) {
		n.handleBlock(ctx, from, blk)
	})
	n.p2p.SetQBFTMsgHandler(n.handleQBFTMsg)
	n.p2p.SetSyncProvider(n.handleSyncRequest)
	n.p2p.SetSyncHandler(n.syncWithPeer)
	if err := n.p2p.Start(ctx, &n.cfg.P2P); err != nil {
		return fmt.Errorf("start P2P: %w", err)
	}

	// 3. Start RPC server in background.
	go func() {
		if err := n.rpc.Start(); err != nil {
			n.log.Error().Err(err).Msg("RPC server error")
		}
	}()

	// 4. Schedule first production slot (Clique) or start QBFT loop (validators only).
	// QBFT followers (no signer key) receive committed blocks via the block gossip
	// topic through handleBlock and do not participate in the protocol loop.
	n.runCtx = ctx
	if _, isBFT := n.cliq.(consensus.BFTEngine); isBFT {
		if n.signerKey != nil {
			go n.runQBFTLoop(ctx)
		}
	} else {
		n.scheduleBlockProduction(ctx)
	}

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

	// Close chain DB.
	if n.db != nil {
		if err := n.db.Close(); err != nil {
			n.log.Warn().Err(err).Msg("chain DB close error")
		}
	}

	n.log.Info().Msg("shutdown complete")
	return nil
}

// SetBootNodes sets the boot node multiaddrs to dial when Start is called.
// Must be called before Start.
func (n *Node) SetBootNodes(addrs []string) {
	n.cfg.P2P.BootNodes = addrs
}

// HeadNumber returns the block number of the current canonical head.
// It is safe to call concurrently and is intended for use by tests and
// external monitors.
func (n *Node) HeadNumber() uint64 {
	h := n.stor.Head()
	if h == nil || h.Number == nil {
		return 0
	}
	return h.Number.Uint64()
}

// P2PAddr returns the first full multiaddr of this node's P2P host including
// the /p2p/<peerID> suffix, suitable for use as a boot node address.
// Returns an empty string if no addresses are available.
func (n *Node) P2PAddr() string {
	if n.p2p == nil {
		return ""
	}
	addrs := n.p2p.Addrs()
	if len(addrs) == 0 {
		return ""
	}
	return fmt.Sprintf("%s/p2p/%s", addrs[0].String(), n.p2p.PeerID().String())
}

// --- Snapshot management ---

// headSnapshot returns the consensus snapshot at the current canonical head.
func (n *Node) headSnapshot() consensus.Snapshot {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.headSnap
}

// setHeadSnapshot updates the head snapshot and caches it at epoch boundaries.
func (n *Node) setHeadSnapshot(snap consensus.Snapshot) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.headSnap = snap
	// Cache epoch-boundary snapshots for efficient reorg recovery.
	if snap.BlockNumber()%n.cliq.Epoch() == 0 {
		n.epochSnaps[snap.BlockNumber()] = snap
	}
}

// computeSnapshot returns the consensus snapshot at the given header by
// advancing from the nearest cached epoch checkpoint.
func (n *Node) computeSnapshot(header *types.Header) (consensus.Snapshot, error) {
	num := header.Number.Uint64()

	// Fast path: extending the current head by one block.
	n.mu.RLock()
	cur := n.headSnap
	n.mu.RUnlock()
	if cur != nil && cur.BlockNumber()+1 == num && cur.BlockHash() == header.ParentHash {
		return n.cliq.Apply(cur, []*types.Header{header})
	}

	// General path: start from the nearest epoch checkpoint ≤ num.
	epoch := n.cliq.Epoch()
	epochStart := (num / epoch) * epoch

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
			base, err = n.cliq.NewCheckpointSnapshot(epochHeader)
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
