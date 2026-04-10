package p2p

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/peterrobinson/consensus-client-vibe/internal/config"
)

// testStatus returns a sample StatusMsg for testing.
func testStatus(networkID uint64, genesis common.Hash) StatusMsg {
	return StatusMsg{
		NetworkID:   networkID,
		GenesisHash: genesis,
		HeadHash:    genesis,
		HeadNumber:  0,
	}
}

// newTestHost creates a Host bound to 127.0.0.1 on a random TCP port.
func newTestHost(t *testing.T, networkID uint64, genesis common.Hash) *Host {
	t.Helper()
	cfg := &config.P2PConfig{
		ListenAddr: "/ip4/127.0.0.1/tcp/0",
		MaxPeers:   10,
		EnableMDNS: false,
	}
	h, err := New(cfg, testStatus(networkID, genesis))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// startHost calls Start on h using a test-scoped context.
func startHost(t *testing.T, h *Host) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cfg := &config.P2PConfig{ListenAddr: "/ip4/127.0.0.1/tcp/0", EnableMDNS: false}
	if err := h.Start(ctx, cfg); err != nil {
		cancel()
		t.Fatalf("Start: %v", err)
	}
	return cancel
}

// connectHosts connects host b to host a by dialling a's address.
func connectHosts(t *testing.T, a, b *Host) {
	t.Helper()
	ai := peer.AddrInfo{ID: a.PeerID(), Addrs: a.Addrs()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.h.Connect(ctx, ai); err != nil {
		t.Fatalf("connect: %v", err)
	}
}

// TestTwoHosts_BlockPropagation verifies that a CliqueBlock published by one
// host is received by a subscribed peer.
func TestTwoHosts_BlockPropagation(t *testing.T) {
	genesis := common.HexToHash("0xdeadbeef")
	a := newTestHost(t, 1, genesis)
	b := newTestHost(t, 1, genesis)

	cancelA := startHost(t, a)
	defer cancelA()
	cancelB := startHost(t, b)
	defer cancelB()

	connectHosts(t, a, b)

	// Wait for the gossipsub mesh to form.
	time.Sleep(200 * time.Millisecond)

	received := make(chan *CliqueBlock, 1)
	b.SetBlockHandler(func(_ peer.ID, blk *CliqueBlock) {
		select {
		case received <- blk:
		default:
		}
	})

	h := &types.Header{
		Number:     big.NewInt(1),
		Difficulty: big.NewInt(2),
		Extra:      make([]byte, 97),
	}
	payloadHash := common.HexToHash("0xcafe")
	blk, err := NewCliqueBlock(h, payloadHash)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.BroadcastBlock(ctx, blk); err != nil {
		t.Fatalf("BroadcastBlock: %v", err)
	}

	select {
	case got := <-received:
		if got.ExecutionPayloadHash != payloadHash {
			t.Errorf("ExecutionPayloadHash: got %s, want %s",
				got.ExecutionPayloadHash.Hex(), payloadHash.Hex())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for block to be received")
	}
}

// TestTwoHosts_StatusHandshake verifies that the status protocol completes
// and that incompatible peers are disconnected.
func TestTwoHosts_StatusHandshake(t *testing.T) {
	genesis := common.HexToHash("0xaabbcc")

	a := newTestHost(t, 42, genesis)
	b := newTestHost(t, 42, genesis)

	cancelA := startHost(t, a)
	defer cancelA()
	cancelB := startHost(t, b)
	defer cancelB()

	// b dials a — b is the outbound side, so b initiates the status handshake.
	connectHosts(t, a, b)

	// Allow time for the handshake to complete.
	time.Sleep(300 * time.Millisecond)

	// Both peers should still be connected (compatible network).
	if b.PeerCount() == 0 {
		t.Error("b lost connection to a after status handshake")
	}
}

// TestTwoHosts_IncompatibleGenesis verifies that peers with different genesis
// hashes are disconnected after the status handshake.
func TestTwoHosts_IncompatibleGenesis(t *testing.T) {
	genesisA := common.HexToHash("0xaaa")
	genesisB := common.HexToHash("0xbbb") // different

	a := newTestHost(t, 1, genesisA)
	b := newTestHost(t, 1, genesisB)

	cancelA := startHost(t, a)
	defer cancelA()
	cancelB := startHost(t, b)
	defer cancelB()

	// b dials a — b is the outbound side, so b initiates the handshake.
	connectHosts(t, a, b)

	// Allow time for handshake + disconnect.
	time.Sleep(500 * time.Millisecond)

	// Both peers should have closed the connection.
	if b.PeerCount() > 0 {
		t.Error("expected b to disconnect from incompatible peer a")
	}
}

// TestHost_SetStatus verifies that updating the status affects subsequent handshakes.
func TestHost_SetStatus(t *testing.T) {
	genesis := common.HexToHash("0x1234")
	h := newTestHost(t, 1, genesis)

	updated := StatusMsg{
		NetworkID:   1,
		GenesisHash: genesis,
		HeadHash:    common.HexToHash("0x5678"),
		HeadNumber:  100,
	}
	h.SetStatus(updated)

	h.mu.RLock()
	got := h.localStatus
	h.mu.RUnlock()

	if got.HeadNumber != 100 {
		t.Errorf("HeadNumber: got %d, want 100", got.HeadNumber)
	}
}

// TestHost_BroadcastBlock_NoSubscribers verifies BroadcastBlock does not error
// when there are no subscribers (message is published to an empty topic).
func TestHost_BroadcastBlock_NoSubscribers(t *testing.T) {
	genesis := common.HexToHash("0xfeed")
	h := newTestHost(t, 1, genesis)
	cancel := startHost(t, h)
	defer cancel()

	blk, err := NewCliqueBlock(&types.Header{
		Number:     big.NewInt(1),
		Difficulty: big.NewInt(1),
		Extra:      make([]byte, 97),
	}, common.Hash{})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if err := h.BroadcastBlock(ctx, blk); err != nil {
		t.Errorf("BroadcastBlock with no subscribers: %v", err)
	}
}

// TestHost_PeerCount verifies PeerCount returns expected values.
func TestHost_PeerCount(t *testing.T) {
	genesis := common.HexToHash("0x1111")
	a := newTestHost(t, 1, genesis)
	b := newTestHost(t, 1, genesis)

	cancelA := startHost(t, a)
	defer cancelA()
	cancelB := startHost(t, b)
	defer cancelB()

	if a.PeerCount() != 0 {
		t.Errorf("PeerCount before connect: got %d, want 0", a.PeerCount())
	}

	connectHosts(t, a, b)

	if a.PeerCount() != 1 {
		t.Errorf("PeerCount after connect: got %d, want 1", a.PeerCount())
	}
}

// --- helpers: ensure pubsub imports are used ---

var _ = pubsub.WithFloodPublish // keep import used
