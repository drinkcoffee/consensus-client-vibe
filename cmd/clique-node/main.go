package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/peterrobinson/consensus-client-vibe/internal/config"
	"github.com/peterrobinson/consensus-client-vibe/internal/log"
	"github.com/urfave/cli/v2"
)

var (
	configFlag = &cli.StringFlag{
		Name:    "config",
		Aliases: []string{"c"},
		Usage:   "Path to TOML configuration file",
		Value:   "config.toml",
		EnvVars: []string{"CLIQUE_CONFIG"},
	}

	logLevelFlag = &cli.StringFlag{
		Name:    "log-level",
		Usage:   "Log level (trace|debug|info|warn|error)",
		Value:   "info",
		EnvVars: []string{"CLIQUE_LOG_LEVEL"},
	}

	logFormatFlag = &cli.StringFlag{
		Name:    "log-format",
		Usage:   "Log format (json|pretty)",
		Value:   "pretty",
		EnvVars: []string{"CLIQUE_LOG_FORMAT"},
	}
)

func main() {
	app := &cli.App{
		Name:    "clique-node",
		Usage:   "Ethereum Clique consensus client",
		Version: "0.1.0",
		Flags: []cli.Flag{
			configFlag,
			logLevelFlag,
			logFormatFlag,
		},
		Action: runNode,
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runNode(cliCtx *cli.Context) error {
	// Initialise logging first so all subsequent output is formatted correctly.
	log.Init(cliCtx.String("log-level"), cliCtx.String("log-format"))
	logger := log.With("main")

	// Load configuration.
	cfgPath := cliCtx.String("config")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		// Fall back to defaults if the config file doesn't exist yet.
		if errors.Is(err, os.ErrNotExist) {
			logger.Warn().Str("path", cfgPath).Msg("Config file not found, using defaults")
			cfg = config.DefaultConfig()
		} else {
			return fmt.Errorf("load config: %w", err)
		}
	}

	// Override log settings from CLI flags if explicitly provided.
	if cliCtx.IsSet("log-level") {
		cfg.Logging.Level = cliCtx.String("log-level")
	}
	if cliCtx.IsSet("log-format") {
		cfg.Logging.Format = cliCtx.String("log-format")
	}
	log.Init(cfg.Logging.Level, cfg.Logging.Format)

	logger.Info().
		Str("config", cfgPath).
		Str("engine_url", cfg.Engine.URL).
		Str("p2p_listen", cfg.P2P.ListenAddr).
		Str("rpc_listen", cfg.RPC.ListenAddr).
		Uint64("network_id", cfg.Node.NetworkID).
		Msg("Starting clique-node")

	// Set up a context that is cancelled on SIGINT / SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// TODO(phase-2+): construct and start the node here.
	// node, err := node.New(cfg)
	// if err != nil { return err }
	// return node.Run(ctx)

	// Placeholder: just block until signal.
	logger.Info().Msg("Node initialised (subsystems not yet wired — see plan.md)")
	<-ctx.Done()
	logger.Info().Msg("Shutdown signal received, stopping")
	return nil
}
