package p2p

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	libp2p "github.com/libp2p/go-libp2p"
	lhost "github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/rs/zerolog"

	"github.com/peterrobinson/consensus-client-vibe/internal/config"
	"github.com/peterrobinson/consensus-client-vibe/internal/log"
)

const (
	// blockTopic is the Gossipsub topic for propagating signed block headers.
	blockTopic = "/consensus/block/1"
	// qbftTopic is the Gossipsub topic for QBFT consensus messages.
	qbftTopic = "/qbft/consensus/1"
	// statusProtocol is the stream protocol ID for the peer status handshake.
	statusProtocol = "/clique/status/1"

	maxStatusMsgSize = 1 * 1024        // 1 KB — status messages are tiny
	statusTimeout    = 10 * time.Second // per-stream deadline for status exchange
)

// BlockHandler is called when a CliqueBlock is received from a remote peer.
type BlockHandler func(from peer.ID, block *CliqueBlock)

// QBFTMsgHandler is called when a QBFTMsg is received from a remote peer.
type QBFTMsgHandler func(msg *QBFTMsg)

// Host wraps a libp2p host with consensus-specific Gossipsub and status protocol.
// It is safe for concurrent use.
type Host struct {
	h    lhost.Host
	ps   *pubsub.PubSub
	bt   *pubsub.Topic
	bsub *pubsub.Subscription
	qt   *pubsub.Topic
	qsub *pubsub.Subscription

	mu              sync.RWMutex
	localStatus     StatusMsg
	blockHandler    BlockHandler
	qbftMsgHandler  QBFTMsgHandler
	syncProvider    SyncProvider
	syncHandler     SyncHandler

	mdnsCloser io.Closer // non-nil when mDNS is running
	log        zerolog.Logger
}

// New creates a Host with a new libp2p peer. Call Start to begin accepting
// connections and join the Gossipsub network.
//
// cfg controls the listen address, boot nodes, and mDNS settings. networkID
// and genesis are included in the StatusMsg sent to every peer.
func New(cfg *config.P2PConfig, initialStatus StatusMsg) (*Host, error) {
	h, err := libp2p.New(
		libp2p.ListenAddrStrings(cfg.ListenAddr),
	)
	if err != nil {
		return nil, fmt.Errorf("create libp2p host: %w", err)
	}

	logger := log.With("p2p")

	logger.Info().
		Str("peer_id", h.ID().String()).
		Strs("addrs", addrsToStrings(h.Addrs())).
		Msg("libp2p host created")

	return &Host{
		h:           h,
		localStatus: initialStatus,
		log:         logger,
	}, nil
}

// Start joins the Gossipsub block topic, registers the status stream handler,
// wires up connection notifications, connects to configured boot nodes, and
// optionally starts mDNS discovery. ctx governs the Gossipsub subscription
// loop; cancel it (or call Close) to stop the host.
func (h *Host) Start(ctx context.Context, cfg *config.P2PConfig) error {
	// Gossipsub.
	ps, err := pubsub.NewGossipSub(ctx, h.h,
		pubsub.WithFloodPublish(true), // ensure delivery even below mesh threshold
	)
	if err != nil {
		return fmt.Errorf("create gossipsub: %w", err)
	}
	h.ps = ps

	bt, err := ps.Join(blockTopic)
	if err != nil {
		return fmt.Errorf("join block topic: %w", err)
	}
	h.bt = bt

	bsub, err := bt.Subscribe()
	if err != nil {
		return fmt.Errorf("subscribe to block topic: %w", err)
	}
	h.bsub = bsub

	// QBFT consensus topic.
	qt, err := ps.Join(qbftTopic)
	if err != nil {
		return fmt.Errorf("join qbft topic: %w", err)
	}
	h.qt = qt

	qsub, err := qt.Subscribe()
	if err != nil {
		return fmt.Errorf("subscribe to qbft topic: %w", err)
	}
	h.qsub = qsub

	// Status stream handler (listener / server side).
	h.h.SetStreamHandler(statusProtocol, h.handleStatusStream)

	// Notify us when outbound connections are established so we can initiate
	// the status handshake. Inbound connections are handled by handleStatusStream.
	h.h.Network().Notify(&network.NotifyBundle{
		ConnectedF: func(_ network.Network, conn network.Conn) {
			if conn.Stat().Direction == network.DirOutbound {
				go h.doStatusHandshake(ctx, conn.RemotePeer())
			}
		},
	})

	// Start background subscription loops.
	go h.subscribeBlocks(ctx)
	go h.subscribeQBFT(ctx)

	// Optional mDNS discovery.
	if cfg.EnableMDNS {
		closer, err := startMDNS(h.h, h.log)
		if err != nil {
			return fmt.Errorf("mDNS: %w", err)
		}
		h.mdnsCloser = closer
		h.log.Info().Msg("mDNS peer discovery enabled")
	}

	// Connect to boot nodes.
	for _, addr := range cfg.BootNodes {
		maddr, err := ma.NewMultiaddr(addr)
		if err != nil {
			h.log.Warn().Err(err).Str("addr", addr).Msg("invalid bootnode multiaddr, skipping")
			continue
		}
		ai, err := peer.AddrInfoFromP2pAddr(maddr)
		if err != nil {
			h.log.Warn().Err(err).Str("addr", addr).Msg("cannot extract peer info from bootnode, skipping")
			continue
		}
		go func(info peer.AddrInfo) {
			if err := h.h.Connect(ctx, info); err != nil {
				h.log.Warn().Err(err).Str("peer", info.ID.String()).Msg("bootnode connect failed")
			} else {
				h.log.Info().Str("peer", info.ID.String()).Msg("connected to bootnode")
			}
		}(*ai)
	}

	h.log.Info().Msg("P2P host started")
	return nil
}

// Close gracefully shuts down the host.
func (h *Host) Close() error {
	if h.bsub != nil {
		h.bsub.Cancel()
	}
	if h.qsub != nil {
		h.qsub.Cancel()
	}
	if h.mdnsCloser != nil {
		_ = h.mdnsCloser.Close()
	}
	return h.h.Close()
}

// SetStatus updates the StatusMsg that is sent to newly connected peers.
func (h *Host) SetStatus(msg StatusMsg) {
	h.mu.Lock()
	h.localStatus = msg
	h.mu.Unlock()
}

// SetBlockHandler registers the callback that is invoked when a CliqueBlock
// arrives from a remote peer via Gossipsub. Replaces any previous handler.
func (h *Host) SetBlockHandler(fn BlockHandler) {
	h.mu.Lock()
	h.blockHandler = fn
	h.mu.Unlock()
}

// SetQBFTMsgHandler registers the callback invoked when a QBFTMsg arrives via
// Gossipsub. Replaces any previous handler.
func (h *Host) SetQBFTMsgHandler(fn QBFTMsgHandler) {
	h.mu.Lock()
	h.qbftMsgHandler = fn
	h.mu.Unlock()
}

// BroadcastQBFTMsg publishes a QBFTMsg to all Gossipsub peers.
func (h *Host) BroadcastQBFTMsg(ctx context.Context, msg *QBFTMsg) error {
	if h.qt == nil {
		return fmt.Errorf("qbft topic not initialised")
	}
	data, err := msg.Encode()
	if err != nil {
		return fmt.Errorf("encode qbft msg: %w", err)
	}
	if err := h.qt.Publish(ctx, data); err != nil {
		return fmt.Errorf("publish qbft msg: %w", err)
	}
	return nil
}

// BroadcastBlock publishes a CliqueBlock to all Gossipsub peers.
func (h *Host) BroadcastBlock(ctx context.Context, block *CliqueBlock) error {
	data, err := block.Encode()
	if err != nil {
		return fmt.Errorf("encode block: %w", err)
	}
	if err := h.bt.Publish(ctx, data); err != nil {
		return fmt.Errorf("publish block: %w", err)
	}
	return nil
}

// PeerID returns the local peer ID.
func (h *Host) PeerID() peer.ID {
	return h.h.ID()
}

// Addrs returns the local listen addresses.
func (h *Host) Addrs() []ma.Multiaddr {
	return h.h.Addrs()
}

// PeerCount returns the number of currently connected peers.
func (h *Host) PeerCount() int {
	return len(h.h.Network().Peers())
}

// ConnectedPeers returns AddrInfo for all connected peers.
func (h *Host) ConnectedPeers() []peer.AddrInfo {
	peers := h.h.Network().Peers()
	infos := make([]peer.AddrInfo, 0, len(peers))
	for _, pid := range peers {
		infos = append(infos, h.h.Peerstore().PeerInfo(pid))
	}
	return infos
}

// --- internal ---

// subscribeBlocks reads CliqueBlock messages from the Gossipsub subscription and
// dispatches them to the registered block handler. Runs until ctx is cancelled.
func (h *Host) subscribeBlocks(ctx context.Context) {
	for {
		msg, err := h.bsub.Next(ctx)
		if err != nil {
			return // ctx cancelled or subscription closed
		}
		if msg.ReceivedFrom == h.h.ID() {
			continue // skip our own published messages
		}
		blk, err := DecodeCliqueBlock(msg.Data)
		if err != nil {
			h.log.Warn().Err(err).
				Str("from", msg.ReceivedFrom.String()).
				Msg("failed to decode block gossip message")
			continue
		}
		h.mu.RLock()
		handler := h.blockHandler
		h.mu.RUnlock()
		if handler != nil {
			handler(msg.ReceivedFrom, blk)
		}
	}
}

// subscribeQBFT reads QBFTMsg messages from the Gossipsub subscription and
// dispatches them to the registered handler. Runs until ctx is cancelled.
func (h *Host) subscribeQBFT(ctx context.Context) {
	for {
		msg, err := h.qsub.Next(ctx)
		if err != nil {
			return // ctx cancelled or subscription closed
		}
		if msg.ReceivedFrom == h.h.ID() {
			continue // skip our own published messages
		}
		qmsg, err := DecodeQBFTMsg(msg.Data)
		if err != nil {
			h.log.Warn().Err(err).
				Str("from", msg.ReceivedFrom.String()).
				Msg("failed to decode qbft message")
			continue
		}
		h.mu.RLock()
		handler := h.qbftMsgHandler
		h.mu.RUnlock()
		if handler != nil {
			handler(qmsg)
		}
	}
}

// doStatusHandshake opens a status stream to pid (dialer side): sends our
// status, then reads the peer's status.
func (h *Host) doStatusHandshake(ctx context.Context, pid peer.ID) {
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()

	s, err := h.h.NewStream(ctx, pid, statusProtocol)
	if err != nil {
		h.log.Debug().Err(err).Str("peer", pid.String()).Msg("open status stream failed")
		return
	}
	defer s.Close()
	_ = s.SetDeadline(time.Now().Add(statusTimeout))

	// Dialer sends first, then reads.
	h.mu.RLock()
	local := h.localStatus
	h.mu.RUnlock()

	if err := writeMsg(s, &local); err != nil {
		h.log.Debug().Err(err).Str("peer", pid.String()).Msg("send status failed")
		_ = s.Reset()
		return
	}

	var remote StatusMsg
	if err := readMsg(s, &remote, maxStatusMsgSize); err != nil {
		h.log.Debug().Err(err).Str("peer", pid.String()).Msg("receive status failed")
		_ = s.Reset()
		return
	}

	h.handlePeerStatus(pid, remote)
}

// handleStatusStream handles an incoming status stream (listener / server side):
// reads the peer's status, then sends our own.
func (h *Host) handleStatusStream(s network.Stream) {
	defer s.Close()
	_ = s.SetDeadline(time.Now().Add(statusTimeout))
	pid := s.Conn().RemotePeer()

	// Listener reads first, then replies.
	var remote StatusMsg
	if err := readMsg(s, &remote, maxStatusMsgSize); err != nil {
		h.log.Debug().Err(err).Str("peer", pid.String()).Msg("read status failed")
		_ = s.Reset()
		return
	}

	h.mu.RLock()
	local := h.localStatus
	h.mu.RUnlock()

	if err := writeMsg(s, &local); err != nil {
		h.log.Debug().Err(err).Str("peer", pid.String()).Msg("write status failed")
		_ = s.Reset()
		return
	}

	h.handlePeerStatus(pid, remote)
}

// handlePeerStatus checks compatibility and logs the outcome.
// Incompatible peers are disconnected. If the peer's head is ahead of ours,
// the registered SyncHandler (if any) is invoked in a new goroutine.
func (h *Host) handlePeerStatus(pid peer.ID, remote StatusMsg) {
	h.mu.RLock()
	local := h.localStatus
	sh := h.syncHandler
	h.mu.RUnlock()

	if remote.NetworkID != local.NetworkID || remote.GenesisHash != local.GenesisHash {
		h.log.Warn().
			Str("peer", pid.String()).
			Uint64("their_network", remote.NetworkID).
			Uint64("our_network", local.NetworkID).
			Str("their_genesis", remote.GenesisHash.Hex()).
			Str("our_genesis", local.GenesisHash.Hex()).
			Msg("peer on incompatible network, disconnecting")
		_ = h.h.Network().ClosePeer(pid)
		return
	}

	h.log.Info().
		Str("peer", pid.String()).
		Uint64("head_number", remote.HeadNumber).
		Str("head_hash", remote.HeadHash.Hex()).
		Msg("peer status exchange complete")

	if sh != nil && remote.HeadNumber > local.HeadNumber {
		go sh(pid)
	}
}

// addrsToStrings converts a slice of multiaddrs to strings for logging.
func addrsToStrings(addrs []ma.Multiaddr) []string {
	ss := make([]string, len(addrs))
	for i, a := range addrs {
		ss[i] = a.String()
	}
	return ss
}
