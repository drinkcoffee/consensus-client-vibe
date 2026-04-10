# Running with Docker

This document covers building and running `clique-node` as a Docker container.

## Building the Image

```bash
docker build -t clique-node .
```

To tag a specific version:

```bash
docker build -t clique-node:v0.1.0 .
```

## Prerequisites

Before starting the container you need two files on the host:

| File | Description |
|---|---|
| `config.toml` | Node configuration (see `config.example.toml`) |
| `jwt.hex` | Shared JWT secret, hex-encoded — must match the one used by your execution client |

The JWT secret can be generated with:

```bash
openssl rand -hex 32 > jwt.hex
```

## Running

Mount the config and JWT secret into the container, and publish the ports you need:

```bash
docker run -d \
  --name clique-node \
  -v /path/to/config.toml:/app/config.toml \
  -v /path/to/jwt.hex:/secrets/jwt.hex \
  -p 9000:9000 \
  -p 5052:5052 \
  clique-node
```

Update `config.toml` to reference the mounted JWT path:

```toml
[engine]
jwt_secret_path = "/secrets/jwt.hex"
```

### Default Exposed Ports

| Port | Protocol | Purpose |
|---|---|---|
| `9000` | TCP | P2P (libp2p) — peer connections and block gossip |
| `5052` | TCP | JSON-RPC HTTP API |
| `8550` | TCP | Engine API (execution client side — not exposed by this container) |

The Engine API port (`8550` / `8551`) is on the **execution client**, not this container. The node connects outbound to it; you do not need to publish it here.

## Connecting to an Execution Client

The execution client must be reachable from inside the container. If both are running on the same host via Docker, put them on a shared network:

```bash
docker network create eth-net

# Start your execution client on eth-net (example: Geth)
docker run -d --name geth --network eth-net \
  -v /path/to/geth-data:/data \
  ethereum/client-go \
  --datadir /data \
  --authrpc.addr 0.0.0.0 \
  --authrpc.jwtsecret /data/jwt.hex \
  --authrpc.vhosts "*"

# Start clique-node on the same network
docker run -d \
  --name clique-node \
  --network eth-net \
  -v /path/to/config.toml:/app/config.toml \
  -v /path/to/jwt.hex:/secrets/jwt.hex \
  -p 9000:9000 \
  -p 5052:5052 \
  clique-node
```

Then in `config.toml` use the execution client's container name as the hostname:

```toml
[engine]
url        = "http://geth:8551"
el_rpc_url = "http://geth:8545"
```

## Docker Compose

A minimal `docker-compose.yml` for running both together:

```yaml
services:
  geth:
    image: ethereum/client-go:latest
    volumes:
      - ./geth-data:/data
    command:
      - --datadir=/data
      - --authrpc.addr=0.0.0.0
      - --authrpc.jwtsecret=/data/jwt.hex
      - --authrpc.vhosts=*
    networks:
      - eth-net

  clique-node:
    image: clique-node:latest
    depends_on:
      - geth
    volumes:
      - ./config.toml:/app/config.toml
      - ./jwt.hex:/secrets/jwt.hex
    ports:
      - "9000:9000"
      - "5052:5052"
    networks:
      - eth-net

networks:
  eth-net:
```

## Overriding Flags

Any `clique-node` flag can be appended after the image name:

```bash
# Enable debug logging
docker run ... clique-node --config /app/config.toml --log-level debug

# JSON log output (recommended for log aggregators)
docker run ... clique-node --config /app/config.toml --log-format json
```

## Viewing Logs

```bash
docker logs -f clique-node
```

## Stopping

```bash
docker stop clique-node   # sends SIGTERM — node shuts down cleanly
docker rm clique-node
```
