package node

import (
	"context"
	"crypto/ecdsa"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	gethcrypto "github.com/ethereum/go-ethereum/crypto"
	libp2ppeer "github.com/libp2p/go-libp2p/core/peer"

	"github.com/peterrobinson/consensus-client-vibe/internal/consensus"
	cliqueeng "github.com/peterrobinson/consensus-client-vibe/internal/consensus/clique"
	"github.com/peterrobinson/consensus-client-vibe/internal/config"
	"github.com/peterrobinson/consensus-client-vibe/internal/engine"
	"github.com/peterrobinson/consensus-client-vibe/internal/forkchoice"
	p2phost "github.com/peterrobinson/consensus-client-vibe/internal/p2p"
	"github.com/peterrobinson/consensus-client-vibe/internal/log"
)

// --- mock Engine API ---

type mockEngine struct {
	mu sync.Mutex

	capsCalled   int
	newPayloadFn func(engine.ExecutionPayloadV3) engine.PayloadStatusV1
	fcuFn        func(engine.ForkchoiceStateV1, *engine.PayloadAttributesV3) engine.ForkchoiceUpdatedResult
	getPayloadFn func(engine.PayloadID) engine.GetPayloadResponseV3

	fcuCalls []struct {
		state engine.ForkchoiceStateV1
		attrs *engine.PayloadAttributesV3
	}
}

func (m *mockEngine) ExchangeCapabilities(_ context.Context) ([]string, error) {
	m.mu.Lock()
	m.capsCalled++
	m.mu.Unlock()
	return []string{"engine_newPayloadV3", "engine_forkchoiceUpdatedV3"}, nil
}

func (m *mockEngine) NewPayloadV3(_ context.Context, p engine.ExecutionPayloadV3, _ []common.Hash, _ common.Hash) (engine.PayloadStatusV1, error) {
	if m.newPayloadFn != nil {
		return m.newPayloadFn(p), nil
	}
	return engine.PayloadStatusV1{Status: engine.PayloadStatusValid}, nil
}

func (m *mockEngine) ForkchoiceUpdatedV3(_ context.Context, state engine.ForkchoiceStateV1, attrs *engine.PayloadAttributesV3) (engine.ForkchoiceUpdatedResult, error) {
	m.mu.Lock()
	m.fcuCalls = append(m.fcuCalls, struct {
		state engine.ForkchoiceStateV1
		attrs *engine.PayloadAttributesV3
	}{state, attrs})
	m.mu.Unlock()

	if m.fcuFn != nil {
		return m.fcuFn(state, attrs), nil
	}
	pid := engine.PayloadID{0x01}
	return engine.ForkchoiceUpdatedResult{
		PayloadStatus: engine.PayloadStatusV1{Status: engine.PayloadStatusValid},
		PayloadID:     &pid,
	}, nil
}

func (m *mockEngine) GetPayloadV3(_ context.Context, _ engine.PayloadID) (engine.GetPayloadResponseV3, error) {
	if m.getPayloadFn != nil {
		return m.getPayloadFn(engine.PayloadID{}), nil
	}
	return engine.GetPayloadResponseV3{
		ExecutionPayload: engine.ExecutionPayloadV3{
			BlockNumber:   hexutil.Uint64(1),
			GasLimit:      hexutil.Uint64(30_000_000),
			Timestamp:     hexutil.Uint64(1000 + 15),
			BaseFeePerGas: (*hexutil.Big)(big.NewInt(7)),
		},
	}, nil
}

// --- test helpers ---

func init() {
	log.Init("warn", "pretty") // suppress log output during tests
}

// testKey generates a deterministic ECDSA key for testing.
func testKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := gethcrypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// makeGenesisHeader creates a genesis header with the given signers in Extra.
func makeGenesisHeader(signers []common.Address) *types.Header {
	extra := make([]byte, cliqueeng.ExtraVanity)
	for _, s := range signers {
		extra = append(extra, s.Bytes()...)
	}
	extra = append(extra, make([]byte, cliqueeng.ExtraSeal)...)
	return &types.Header{
		Number:     big.NewInt(0),
		Difficulty: big.NewInt(1),
		Extra:      extra,
		Time:       1000,
	}
}

// nodeForTest builds a minimal Node suitable for unit tests, bypassing the
// ethclient genesis fetch. It uses the provided genesis header directly.
func nodeForTest(t *testing.T, genesis *types.Header, signerKey *ecdsa.PrivateKey, eng EngineAPI) *Node {
	t.Helper()

	cliq := cliqueeng.New(15, 100)
	genesisSnap, err := cliq.NewGenesisSnapshot(genesis)
	if err != nil {
		t.Fatalf("genesis snapshot: %v", err)
	}

	cfg := &config.Config{
		Node:      config.NodeConfig{NetworkID: 1},
		Consensus: config.ConsensusConfig{Clique: config.CliqueConfig{Period: 15, Epoch: 100}},
		P2P:       config.P2PConfig{ListenAddr: "/ip4/127.0.0.1/tcp/0"},
		RPC:       config.RPCConfig{},
	}

	stor := forkchoice.New(genesis, cliq.Epoch())

	var signerAddr common.Address
	if signerKey != nil {
		signerAddr = gethcrypto.PubkeyToAddress(signerKey.PublicKey)
	}

	n := &Node{
		cfg:         cfg,
		eng:         eng,
		stor:        stor,
		cliq:        cliq,
		signerKey:   signerKey,
		signerAddr:  signerAddr,
		genesisSnap: genesisSnap,
		headSnap:    genesisSnap,
		epochSnaps:  make(map[uint64]consensus.Snapshot),
		log:         log.With("test-node"),
	}
	return n
}

// --- scheduleBlockProduction ---

func TestSchedule_FollowerMode(t *testing.T) {
	key := testKey(t)
	genesis := makeGenesisHeader([]common.Address{gethcrypto.PubkeyToAddress(key.PublicKey)})
	n := nodeForTest(t, genesis, nil, &mockEngine{}) // no signer key → follower

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n.scheduleBlockProduction(ctx) // should be a no-op

	n.prodMu.Lock()
	timer := n.prodTimer
	n.prodMu.Unlock()
	if timer != nil {
		t.Error("expected no timer in follower mode")
	}
}

func TestSchedule_NotAuthorized(t *testing.T) {
	signer := testKey(t)
	outsider := testKey(t)
	genesis := makeGenesisHeader([]common.Address{gethcrypto.PubkeyToAddress(signer.PublicKey)})
	// outsider is not in the signer set
	n := nodeForTest(t, genesis, outsider, &mockEngine{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n.scheduleBlockProduction(ctx)

	n.prodMu.Lock()
	timer := n.prodTimer
	n.prodMu.Unlock()
	if timer != nil {
		t.Error("expected no timer when signer is not authorized")
	}
}

func TestSchedule_InTurn(t *testing.T) {
	// Single signer — always in-turn.
	key := testKey(t)
	genesis := makeGenesisHeader([]common.Address{gethcrypto.PubkeyToAddress(key.PublicKey)})
	// Set genesis time in the past so delay is near 0.
	genesis.Time = uint64(time.Now().Unix()) - 30 // 30s ago

	n := nodeForTest(t, genesis, key, &mockEngine{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	n.scheduleBlockProduction(ctx)

	n.prodMu.Lock()
	timer := n.prodTimer
	n.prodMu.Unlock()
	if timer == nil {
		t.Error("expected a production timer for in-turn signer")
	}
}

func TestSchedule_ReplacesOldTimer(t *testing.T) {
	key := testKey(t)
	genesis := makeGenesisHeader([]common.Address{gethcrypto.PubkeyToAddress(key.PublicKey)})
	genesis.Time = uint64(time.Now().Unix()) - 30

	n := nodeForTest(t, genesis, key, &mockEngine{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Schedule twice — second call should replace the first timer.
	n.scheduleBlockProduction(ctx)
	n.prodMu.Lock()
	first := n.prodTimer
	n.prodMu.Unlock()

	n.scheduleBlockProduction(ctx)
	n.prodMu.Lock()
	second := n.prodTimer
	n.prodMu.Unlock()

	if first == second {
		t.Error("expected timer to be replaced on second schedule call")
	}
}

// --- handleBlock ---

func TestHandleBlock_UnknownParent(t *testing.T) {
	key := testKey(t)
	genesis := makeGenesisHeader([]common.Address{gethcrypto.PubkeyToAddress(key.PublicKey)})
	n := nodeForTest(t, genesis, nil, &mockEngine{})
	ctx := context.Background()

	// Build a header whose parent is not in the store.
	orphan := &types.Header{
		Number:     big.NewInt(1),
		ParentHash: common.HexToHash("0xdeadbeef"), // unknown
		Difficulty: big.NewInt(2),
		Extra:      make([]byte, cliqueeng.ExtraVanity+cliqueeng.ExtraSeal),
		Time:       1015,
	}
	blk, _ := p2phost.NewCliqueBlock(orphan, engine.ExecutionPayloadV3{})
	initialLen := n.stor.Len()

	n.handleBlock(ctx, libp2ppeer.ID(""), blk)

	if n.stor.Len() != initialLen {
		t.Error("store should not grow when parent is unknown")
	}
}

func TestHandleBlock_Valid(t *testing.T) {
	key := testKey(t)
	signerAddr := gethcrypto.PubkeyToAddress(key.PublicKey)
	genesis := makeGenesisHeader([]common.Address{signerAddr})
	genesis.Time = 1000

	mockEng := &mockEngine{}
	n := nodeForTest(t, genesis, nil, mockEng)
	ctx := context.Background()

	// Build a valid block 1.
	h := &types.Header{
		Number:     big.NewInt(1),
		ParentHash: genesis.Hash(),
		Difficulty: big.NewInt(cliqueeng.DiffInTurn),
		Extra:      make([]byte, cliqueeng.ExtraVanity+cliqueeng.ExtraSeal),
		UncleHash:  cliqueeng.EmptyUncleHash,
		Time:       1000 + 15,
	}
	if err := cliqueeng.SealHeader(h, key); err != nil {
		t.Fatal(err)
	}

	blk, err := p2phost.NewCliqueBlock(h, engine.ExecutionPayloadV3{BlockHash: h.Hash()})
	if err != nil {
		t.Fatal(err)
	}

	n.handleBlock(ctx, libp2ppeer.ID(""), blk)

	// Block should now be in the store and be the new head.
	if n.stor.Len() != 2 { // genesis + block 1
		t.Errorf("store len = %d, want 2", n.stor.Len())
	}
	head := n.stor.Head()
	if head == nil || head.Number.Uint64() != 1 {
		t.Errorf("expected head at block 1, got %v", head)
	}

	// FCU should have been called once.
	mockEng.mu.Lock()
	calls := len(mockEng.fcuCalls)
	mockEng.mu.Unlock()
	if calls != 1 {
		t.Errorf("FCU call count = %d, want 1", calls)
	}
}

func TestHandleBlock_InvalidSignature(t *testing.T) {
	key := testKey(t)
	outsider := testKey(t)
	signerAddr := gethcrypto.PubkeyToAddress(key.PublicKey)
	genesis := makeGenesisHeader([]common.Address{signerAddr})
	genesis.Time = 1000

	n := nodeForTest(t, genesis, nil, &mockEngine{})
	ctx := context.Background()

	// Signed by an outsider — should be rejected.
	h := &types.Header{
		Number:     big.NewInt(1),
		ParentHash: genesis.Hash(),
		Difficulty: big.NewInt(cliqueeng.DiffNoTurn),
		Extra:      make([]byte, cliqueeng.ExtraVanity+cliqueeng.ExtraSeal),
		UncleHash:  cliqueeng.EmptyUncleHash,
		Time:       1000 + 15,
	}
	if err := cliqueeng.SealHeader(h, outsider); err != nil {
		t.Fatal(err)
	}

	blk, _ := p2phost.NewCliqueBlock(h, engine.ExecutionPayloadV3{BlockHash: h.Hash()})
	n.handleBlock(ctx, libp2ppeer.ID(""), blk)

	// Block should NOT be in the store.
	if n.stor.Len() != 1 {
		t.Errorf("store len = %d, want 1 (only genesis)", n.stor.Len())
	}
}

// --- buildExtra ---

func TestBuildExtra_NonEpoch(t *testing.T) {
	key := testKey(t)
	genesis := makeGenesisHeader([]common.Address{gethcrypto.PubkeyToAddress(key.PublicKey)})
	n := nodeForTest(t, genesis, key, &mockEngine{})

	snap, _ := n.cliq.NewGenesisSnapshot(genesis)
	extra := n.cliq.BuildExtra(snap, 1) // block 1 is not an epoch block

	wantLen := cliqueeng.ExtraVanity + cliqueeng.ExtraSeal
	if len(extra) != wantLen {
		t.Errorf("non-epoch extra len = %d, want %d", len(extra), wantLen)
	}
}

func TestBuildExtra_Epoch(t *testing.T) {
	key := testKey(t)
	signerAddr := gethcrypto.PubkeyToAddress(key.PublicKey)
	genesis := makeGenesisHeader([]common.Address{signerAddr})
	n := nodeForTest(t, genesis, key, &mockEngine{})
	n.cliq = cliqueeng.New(15, 5) // override with epoch=5

	snap, _ := n.cliq.NewGenesisSnapshot(genesis)
	extra := n.cliq.BuildExtra(snap, 5) // block 5 = epoch boundary (epoch=5)

	// ExtraVanity + 1×20 bytes (one signer) + ExtraSeal
	wantLen := cliqueeng.ExtraVanity + 20 + cliqueeng.ExtraSeal
	if len(extra) != wantLen {
		t.Errorf("epoch extra len = %d, want %d", len(extra), wantLen)
	}
}

// --- computeSnapshot ---

func TestComputeSnapshot_GenesisBlock(t *testing.T) {
	key := testKey(t)
	genesis := makeGenesisHeader([]common.Address{gethcrypto.PubkeyToAddress(key.PublicKey)})
	n := nodeForTest(t, genesis, nil, &mockEngine{})

	snap, err := n.computeSnapshotAt(genesis)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.IsAuthorized(gethcrypto.PubkeyToAddress(key.PublicKey)) {
		t.Error("signer should be authorized in genesis snapshot")
	}
}
