# Docker deployment

This guide covers the single CallHarbor container image: building it, running it with one persistent volume, checking health and stopping it safely. It documents only behavior that exists in the current `main` branch.

Not covered here yet: Docker Compose, reverse proxy and WebSocket (WSS) ingress, Telnyx and AI gateway setup, backups, restore, upgrades and the web UI. These are tracked by their feature issues and will be documented when they ship.

## Image

The root `Dockerfile` builds a static Go binary and copies it into `gcr.io/distroless/static-debian12:nonroot`.

| Property | Value |
| --- | --- |
| Entrypoint | `/usr/local/bin/callharbor` |
| User | `nonroot:nonroot` (UID/GID 65532) |
| Exposed port | `8080/tcp` |
| Volume | `/data` |
| Environment defaults | `CALLHARBOR_LISTEN_ADDR=:8080`, `CALLHARBOR_DATA_DIR=/data` |

The distroless runtime has no shell, package manager or `id` binary. Use `docker inspect` to check the user, not `docker exec`.

## Build

```sh
docker build -t callharbor:local .
```

## Run

Create a named volume for the data directory and start the container:

```sh
docker volume create callharbor-data
docker run -d \
  --name callharbor \
  --restart unless-stopped \
  -p 127.0.0.1:8080:8080 \
  -v callharbor-data:/data \
  callharbor:local
```

The port is bound to `127.0.0.1` so the service is not reachable from other hosts. Put a TLS reverse proxy in front of it before exposing it to a network. That setup is not yet documented.

The nonroot process must be able to write to the data directory. The image creates `/data` owned by `nonroot`. If you use a bind mount instead, the host directory must be writable by UID 65532. Bind-mount ownership has not been tested yet. At startup the server checks that `CALLHARBOR_DATA_DIR` exists, is a directory and is writable. If any check fails, it exits with an error that names the directory.

## Configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `CALLHARBOR_LISTEN_ADDR` | `:8080` | `host:port` to listen on. The port must be 1 to 65535. |
| `CALLHARBOR_DATA_DIR` | `/data` | Absolute path to the data directory. |
| `CALLHARBOR_LOG_LEVEL` | `info` | One of `debug`, `info`, `warn` or `error`. |
| `CALLHARBOR_SHUTDOWN_TIMEOUT` | `15s` | Go duration greater than 0 and at most `5m`. |

An invalid value stops the container at startup with a message that names the variable. Logs are JSON lines on stdout.

## Health checks

| Endpoint | Success | Meaning |
| --- | --- | --- |
| `GET /healthz` | `200 {"status":"ok"}` | The process is alive. |
| `GET /readyz` | `200 {"status":"ready"}` | The server accepts requests. It returns `503 {"status":"unavailable"}` once shutdown has started. |

```sh
curl -fsS http://127.0.0.1:8080/healthz
curl -fsS http://127.0.0.1:8080/readyz
```

Both responses have `Cache-Control: no-store`. Every response sets `Content-Security-Policy`, `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY` and `Referrer-Policy: no-referrer`.

The image is distroless and contains no shell, so a Docker `HEALTHCHECK` or `--health-cmd` that runs commands inside the container cannot use these endpoints as-is. Use an external monitor that calls the endpoints over HTTP.

## Stop and restart

```sh
docker stop callharbor
docker start callharbor
```

On `SIGTERM`, the server sets `/readyz` to unavailable, stops accepting new connections and waits up to `CALLHARBOR_SHUTDOWN_TIMEOUT` for in-flight requests. It then exits.

Data stays in the named volume. `docker rm` removes the container, not the volume. Removing the volume with `docker volume rm callharbor-data` deletes all stored data.

## Troubleshooting

- **Container exits right away with `data directory ... is not writable`:** the volume or bind mount is not writable by UID 65532. Fix the ownership of the host directory, or recreate the named volume.
- **Container exits with `invalid configuration`:** one of the `CALLHARBOR_*` variables is invalid. The message names it.
- **`/readyz` returns 503:** the server is shutting down. Check `docker logs callharbor`.
- **Port already in use:** another process uses host port 8080. Change the host side of `-p`, for example `-p 127.0.0.1:18080:8080`.

## Verification

The commands above were checked against commit `1b8faad` on `main`:

- `docker build` succeeded.
- The image reports `User` as `nonroot:nonroot`, `/data` as a volume and `8080/tcp` as exposed.
- With a named volume, `/healthz` returned `200 {"status":"ok"}` and `/readyz` returned `200 {"status":"ready"}`.
- The probe container, volume and image were removed afterwards.

Not yet verified: the full clean-install run of these commands from a fresh checkout, the bind-mount ownership case, and the shutdown behavior under `docker stop`.
