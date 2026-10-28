# Sode VEP Bridge

Makes a **Blackmagic Videohub** look like an **SW-P-08 router** to [Sode](https://github.com/sethdotfm/sode).

**Version:** Beta 1.0

The bridge polls the Videohub's names, routes and locks, reports them to Sode, and passes Sode's takes on to the hub. A one-page GUI on port 8081 shows status, the live crosspoints and the settings.

- Single static Go binary in a small distroless image. Plain HTTP on :8081 (put your own TLS proxy in front) and SW-P-08 on :8910.
- Videohub input/output *n* is SW-P-08 source/destination *n*: video only, one level.
- Locked outputs are reported as protected. If the hub goes away, Sode sees the router go down.
- Themed with [patchbay](https://github.com/sethdotfm/patchbay), vendored and embedded: no external fetches except the page font.

## Run

`compose.yaml`:

```yaml
services:
  vep:
    image: ghcr.io/sethdotfm/sode-vep:latest
    restart: unless-stopped
    ports: ["8081:8081", "8910:8910"]
    volumes: ["vep-data:/data"]
    environment:
      VEP_HUB: 10.0.0.60   # your Videohub's address

volumes:
  vep-data:
```

```sh
docker compose up -d     # then open http://localhost:8081
```

Settings are saved to `/data/config.yaml`. `VEP_*` variables only seed that file on first start; after that, change them on the GUI. (If you tried the simulator first, the file already says `hub: vhsim`: set the address on the GUI, or `docker compose down -v`.)

To try it without a Videohub, run from a checkout: `VEP_HUB=vhsim docker compose --profile sim --profile sode up --build` starts a fake Videohub, the bridge and Sode.

## Point Sode at it

| Sode setting | Value |
|---|---|
| `SODE_ROUTER` | `<bridge host>:8910` |
| `SODE_LEVEL_COUNT` | `1` |
| `SODE_LEVEL_WIRE_OFFSET` | `-1` (UI level 1 is wire level 0) |
| `SODE_NAME_LEN` | `16` (the most SW-P-08 carries) |

Any matrix number works.

## Settings

| GUI / env | Default | |
|---|---|---|
| Videohub address, `VEP_HUB` | none | host or host:port (port defaults to 9990) |
| Protect device, `VEP_PROTECT_DEVICE` | 1 | SW-P-08 device number reported for locked outputs |
| Resync seconds, `VEP_RESYNC_SECONDS` | 60 | how often names, routes and locks are re-read |
| `VEP_DEBUG` | false | log every line sent to and read from the Videohub (env only: `docker compose logs vep`) |
| `VEP_CONFIG` | `/data/config.yaml` | settings file |
| `VEP_LISTEN_HTTP`, `VEP_LISTEN_SWP08` | `:8081`, `:8910` | listen addresses |

Compose host ports: `VEP_GUI_PORT`, `VEP_SWP08_PORT`, `SODE_PORT`, `VHSIM_PORT`.

## How it behaves

- A take from Sode goes to the hub. The route change comes back from the hub and is announced to Sode. A take that changes nothing, or that the hub refuses, gets no reply (as a real router does), so Sode reports it by its own timeout.
- Takes to locked outputs are not forwarded. Sode cannot change locks.
- If the hub is unreachable, the bridge drops Sode's connection and refuses new ones until the hub is back, so Sode shows the router as down rather than stale names.
- Names are cut to the length Sode asks for (16 at most) and non-ASCII characters are folded. The GUI shows anything past 16 characters in red.

## Development

See [AGENTS.md](AGENTS.md). `go test ./...`, `node --test 'web/test/*.test.mjs'`, `go run ./cmd/vhsim`. Protocol notes: [docs/videohub-protocol.md](docs/videohub-protocol.md).

Releases: bump `internal/version/version.go` and the `**Version:**` line above together, then push a `v1.0`-style tag. The `image` workflow checks the tag against the code and publishes a multi-arch image to `ghcr.io/sethdotfm/sode-vep`.

## Testing on a real Videohub

This is a beta: it has been tried against a real Videohub 12G 40x40, but not across models or firmware. Record what you confirm in the "Verified against the real Videohub" section of AGENTS.md. Still open: whether the hub sends `END PRELUDE:`, how a take to an output locked by another client is answered, label length and encoding, and the hub's client limit.

Unlicense, see LICENSE.
