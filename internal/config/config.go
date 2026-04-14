package config

import (
	"fmt"
	"os"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the top-level configuration for the clique-node.
type Config struct {
	Node      NodeConfig      `toml:"node"`
	Engine    EngineConfig    `toml:"engine"`
	P2P       P2PConfig       `toml:"p2p"`
	RPC       RPCConfig       `toml:"rpc"`
	Consensus ConsensusConfig `toml:"consensus"`
	Logging   LoggingConfig   `toml:"logging"`
}

// NodeConfig holds general node settings.
type NodeConfig struct {
	// DataDir is the directory for persistent node data (e.g. peer store).
	DataDir string `toml:"data_dir"`
	// NetworkID identifies the network (must match the execution client).
	NetworkID uint64 `toml:"network_id"`
}

// EngineConfig holds settings for the Engine API connection to the execution client.
type EngineConfig struct {
	// URL is the Engine API endpoint of the execution client, e.g. http://localhost:8551.
	URL string `toml:"url"`
	// JWTSecretPath is the path to the hex-encoded JWT shared secret file.
	JWTSecretPath string `toml:"jwt_secret_path"`
	// ELRPCUrl is the standard JSON-RPC endpoint of the execution client (no JWT auth),
	// used to fetch the genesis block on startup. Defaults to http://localhost:8545.
	ELRPCUrl string `toml:"el_rpc_url"`
	// DialTimeout is how long to wait when connecting to the execution client.
	DialTimeout duration `toml:"dial_timeout"`
	// CallTimeout is the per-call timeout for Engine API requests.
	CallTimeout duration `toml:"call_timeout"`
}

// P2PConfig holds settings for the libp2p networking layer.
type P2PConfig struct {
	// ListenAddr is the multiaddr to listen on, e.g. /ip4/0.0.0.0/tcp/9000.
	ListenAddr string `toml:"listen_addr"`
	// BootNodes is a list of multiaddrs for initial peers.
	BootNodes []string `toml:"boot_nodes"`
	// MaxPeers is the maximum number of connected peers.
	MaxPeers int `toml:"max_peers"`
	// EnableMDNS enables mDNS peer discovery (useful for local testing).
	EnableMDNS bool `toml:"enable_mdns"`
}

// RPCConfig holds settings for the JSON-RPC HTTP server.
type RPCConfig struct {
	// ListenAddr is the address and port to bind, e.g. 0.0.0.0:5052.
	ListenAddr string `toml:"listen_addr"`
	// ReadTimeout is the HTTP read timeout.
	ReadTimeout duration `toml:"read_timeout"`
	// WriteTimeout is the HTTP write timeout.
	WriteTimeout duration `toml:"write_timeout"`
}

// ConsensusConfig selects the consensus mechanism and holds its parameters.
type ConsensusConfig struct {
	// Type is the consensus mechanism to use. Currently only "clique" is supported.
	// Defaults to "clique" when empty.
	Type   string       `toml:"type"`
	Clique CliqueConfig `toml:"clique"`
}

// CliqueConfig holds Clique consensus parameters.
type CliqueConfig struct {
	// SignerKeyPath is the path to the hex-encoded ECDSA private key used to sign blocks.
	// If empty, the node operates in non-signing (follower) mode.
	SignerKeyPath string `toml:"signer_key_path"`
	// Period is the minimum time between blocks in seconds (matches genesis config).
	Period uint64 `toml:"period"`
	// Epoch is the number of blocks after which to checkpoint and reset votes (matches genesis config).
	Epoch uint64 `toml:"epoch"`
	// GenesisHash is the expected genesis block hash (hex, with 0x prefix).
	GenesisHash string `toml:"genesis_hash"`
}

// LoggingConfig holds logging settings.
type LoggingConfig struct {
	// Level is one of: trace, debug, info, warn, error.
	Level string `toml:"level"`
	// Format is one of: json, pretty.
	Format string `toml:"format"`
}

// duration is a wrapper around time.Duration that supports TOML unmarshalling
// from a string like "5s" or "1m30s".
type duration struct {
	time.Duration
}

func (d *duration) UnmarshalText(text []byte) error {
	var err error
	d.Duration, err = time.ParseDuration(string(text))
	return err
}

// DefaultConfig returns a Config populated with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		Node: NodeConfig{
			DataDir:   "./data",
			NetworkID: 1,
		},
		Engine: EngineConfig{
			URL:           "http://localhost:8551",
			JWTSecretPath: "./jwt.hex",
			ELRPCUrl:      "http://localhost:8545",
			DialTimeout:   duration{10 * time.Second},
			CallTimeout:   duration{5 * time.Second},
		},
		P2P: P2PConfig{
			ListenAddr: "/ip4/0.0.0.0/tcp/9000",
			MaxPeers:   50,
			EnableMDNS: false,
		},
		RPC: RPCConfig{
			ListenAddr:   "0.0.0.0:5052",
			ReadTimeout:  duration{10 * time.Second},
			WriteTimeout: duration{10 * time.Second},
		},
		Consensus: ConsensusConfig{
			Type: "clique",
			Clique: CliqueConfig{
				Period: 15,
				Epoch:  30000,
			},
		},
		Logging: LoggingConfig{
			Level:  "info",
			Format: "pretty",
		},
	}
}

// Load reads a TOML config file from path, overlaying values onto the defaults.
func Load(path string) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file %q: %w", path, err)
	}

	if _, err := toml.Decode(string(data), cfg); err != nil {
		return nil, fmt.Errorf("parse config file %q: %w", path, err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return cfg, nil
}

// Validate checks that required fields are set and values are sensible.
func (c *Config) Validate() error {
	if c.Engine.URL == "" {
		return fmt.Errorf("engine.url must be set")
	}
	if c.Engine.JWTSecretPath == "" {
		return fmt.Errorf("engine.jwt_secret_path must be set")
	}
	if c.Consensus.Clique.Epoch == 0 {
		return fmt.Errorf("consensus.clique.epoch must be > 0")
	}
	if c.P2P.MaxPeers <= 0 {
		return fmt.Errorf("p2p.max_peers must be > 0")
	}
	return nil
}
