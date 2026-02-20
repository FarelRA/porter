# Porter

Porter is a small TCP/UDP tunnel (port forwarder) with a built-in web UI.

It listens on host ports and forwards traffic to either:
- a Docker container (resolved to its IP via the Docker API), or
- a host/hostname.

The UI is embedded into the binary and served from the same HTTP server.

## Quick start

Run the binary:

```bash
./porter
```

Then open:

```text
http://<host>:9876
```

## Docker / GHCR

Image:

```text
ghcr.io/farelra/porter:latest
```

Example:

```bash
podman run --rm -p 9876:9876 -v ./config:/config ghcr.io/farelra/porter:latest
```

## Configuration

Porter stores configuration as JSON at `PORTER_CONFIG_PATH`.

Defaults:
- `PORTER_HTTP_ADDR=:9876`
- `PORTER_DATA_DIR=/config`
- `PORTER_CONFIG_PATH=$PORTER_DATA_DIR/porter.json`

Schema:

```json
{
  "rules": [
    {
      "id": "deadbeef",
      "name": "my-forward",
      "protocol": "tcp",
      "listen_host": "0.0.0.0",
      "listen_port": 8080,
      "target_type": "container",
      "target_container": "umbrel",
      "target_network": "",
      "target_port": 80,
      "enabled": true
    }
  ]
}
```

Notes:
- `id` is generated if omitted; IDs must match `[A-Za-z0-9._-]` and must not contain slashes.
- `listen_host` defaults to `0.0.0.0`.
- `target_type` is `container` or `host`.

## API authentication

If `PORTER_API_TOKEN` is set, all `/api/*` endpoints require:

```text
Authorization: Bearer <token>
```

The UI will prompt for the token when it receives `401 Unauthorized`.

## Performance notes

Porter is designed for long-lived tunnels:
- TCP forwarding uses concurrent bidirectional streaming.
- Copying prefers zero-copy fast paths when available (`io.WriterTo` / `io.ReaderFrom`, used by `net.TCPConn`).
- When no fast path is available, Porter uses pooled 64KiB buffers to reduce allocations.
- UDP forwarding uses a short-lived target address cache to avoid resolving the same target on every packet.

## Development

```bash
go test ./...
```
