# Documentation

Technical reference for `consensus-client-vibe`, a Clique Proof-of-Authority Ethereum consensus client.

## Contents

| Document | Description |
|---|---|
| [Architecture.md](Architecture.md) | Overall system design, component descriptions, data flow diagrams, and package layout |
| [Engine.md](Engine.md) | Engine API integration: JWT authentication, all JSON-RPC methods, call sequences, and error handling |
| [P2P.md](P2P.md) | P2P networking: wire message formats, transport stack, Gossipsub topic, status handshake protocol, and peer discovery |
| [RPC.md](RPC.md) | JSON-RPC HTTP API: all endpoints with request parameters, response schemas, and example payloads |

## Quick Links

**I want to understand how the node is structured** → [Architecture.md](Architecture.md)

**I want to understand how the node talks to Geth/Nethermind** → [Engine.md](Engine.md)

**I want to understand how nodes find and communicate with each other** → [P2P.md](P2P.md)

**I want to query or control a running node** → [RPC.md](RPC.md)
