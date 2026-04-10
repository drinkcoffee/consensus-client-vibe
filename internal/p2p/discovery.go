package p2p

import (
	"context"
	"fmt"
	"io"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"
	"github.com/rs/zerolog"
)

const mdnsServiceTag = "clique-consensus"

// mdnsNotifee receives mDNS peer discovery events and connects to found peers.
type mdnsNotifee struct {
	h   host.Host
	log zerolog.Logger
}

// HandlePeerFound is called by the mDNS service when a new peer is discovered
// on the local network.
func (n *mdnsNotifee) HandlePeerFound(info peer.AddrInfo) {
	if info.ID == n.h.ID() {
		return // skip ourselves
	}
	n.log.Debug().Str("peer", info.ID.String()).Msg("mDNS: discovered peer, connecting")
	if err := n.h.Connect(context.Background(), info); err != nil {
		n.log.Debug().Err(err).Str("peer", info.ID.String()).Msg("mDNS: connect failed")
	}
}

// startMDNS starts an mDNS discovery service for local peer discovery.
// Returns an io.Closer that stops the service.
func startMDNS(h host.Host, log zerolog.Logger) (io.Closer, error) {
	notifee := &mdnsNotifee{h: h, log: log}
	svc := mdns.NewMdnsService(h, mdnsServiceTag, notifee)
	if err := svc.Start(); err != nil {
		return nil, fmt.Errorf("start mDNS service: %w", err)
	}
	return svc, nil
}
