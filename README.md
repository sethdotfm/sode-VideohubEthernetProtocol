# sode-vep

A small bridge that makes one **Blackmagic Videohub** look like an **SW-P-08 router** to [Sode](https://github.com/sethdotfm/sode). It polls the hub's names, routes and locks, reports them to Sode, and passes Sode's takes on to the hub. A one-page GUI on port 8081 shows status and the live route table.

**Version:** 0.1.0 (MVP, not yet verified on a real Videohub)

## Run

```sh
VEP_HUB=10.0.0.60 docker compose up -d vep        # your Videohub's IP
```

Then open http://localhost:8081. Settings are saved to `/data/config.yaml` (a volume); `VEP_*` variables only seed the file on first start. **If you tried the sim profile first, the file already says `hub: vhsim` and `VEP_HUB` is ignored:** set the address on the GUI, or run `docker compose down -v` to start clean.

Try it without hardware: `VEP_HUB=vhsim docker compose --profile sim --profile sode up --build` starts a fake Videohub, the bridge and the real Sode image (Sode on :8080).

## Point Sode at it

In Sode, use the bridge as the router and give it **one level**:

| Sode setting | Value |
|---|---|
| `SODE_ROUTER` | `<bridge host>:8910` |
| `SODE_LEVEL_COUNT` | `1` |
| `SODE_LEVEL_WIRE_OFFSET` | `-1` (UI level 1 → wire level 0) |
| `SODE_NAME_LEN` | `12` (longer names are cut) |

Matrix number doesn't matter (any is accepted).

## Settings

| GUI / env | Default | |
|---|---|---|
| Videohub address `VEP_HUB` | none | host or host:port (port defaults to 9990) |
| Protect device `VEP_PROTECT_DEVICE` | 1 | SW-P-08 device number reported for locked outputs |
| Resync seconds `VEP_RESYNC_SECONDS` | 60 | how often names, routes and locks are re-polled |
| `VEP_DEBUG` | false | log the Videohub wire traffic |
| `VEP_CONFIG` | `/data/config.yaml` in the image | settings file |
| `VEP_LISTEN_HTTP` / `VEP_LISTEN_SWP08` | `:8081` / `:8910` | listen addresses |

Compose host ports: `VEP_GUI_PORT`, `VEP_SWP08_PORT`, `SODE_PORT`, `VHSIM_PORT`.

## How it behaves

- Videohub input/output *n* is SW-P-08 source/destination *n* (both 0-based), video only, on level 0.
- Takes from Sode are sent to the hub. The route change comes back from the hub and is announced to Sode as Connected. A take that changes nothing, or that the hub refuses, gets no reply (as a real router does), so Sode reports it by its own timeout.
- Outputs locked on the hub (`L`, or `O` by this bridge) are reported as **protected**. Takes to them are not forwarded. Sode cannot change locks.
- If the hub is unreachable, the bridge drops Sode's connection and refuses new ones until the hub is back, so Sode shows the router as down rather than showing stale names.
- Labels are cut to the length Sode asks for and non-ASCII characters are folded.

## Development

See [AGENTS.md](AGENTS.md). `go test ./...`, `node --test 'web/test/*.test.mjs'`, `go run ./cmd/vhsim`. Protocol notes: [docs/videohub-protocol.md](docs/videohub-protocol.md).

## Testing on a real Videohub

Fill in the "Verified against the real Videohub" section of AGENTS.md as you go. Things to confirm: the model's prelude (does it send `END PRELUDE:`?), take ACK/NAK and the update that follows, a take to an output locked by another client, label length and encoding, and how many clients the hub allows.

Unlicense, see LICENSE.
