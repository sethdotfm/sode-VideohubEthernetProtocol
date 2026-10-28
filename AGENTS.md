# AGENTS.md

Guidance for coding agents in this repo. (`CLAUDE.md` is only an `@AGENTS.md` import.)

# sode-vep — notes

Bridge that makes one Blackmagic Videohub look like an SW-P-08 router to [Sode](https://github.com/sethdotfm/sode). It polls the hub's names, routes and locks, serves them to Sode as SW-P-08, passes Sode's takes on to the hub, and shows status on a one-page GUI on :8081. Full design: `docs/superpowers/plans/2026-10-28-videohub-bridge.md`. User-facing settings: README.md.

## Commands

```sh
go test ./...        # all tests (the Dockerfile build runs them too)
go vet ./...
go run ./cmd/vhsim                          # fake Videohub on :9990
VEP_HUB=127.0.0.1 go run ./cmd/vep          # SW-P-08 on :8910, GUI on :8081
docker compose --profile sim up --build     # bridge + fake hub
docker compose --profile sim --profile sode up --build   # + real Sode on :8080
```

Without `VEP_CONFIG`, config comes from `VEP_*` env and is not persisted. The web UI is embedded with `go:embed`: restart after frontend edits.

## Key decisions

- Go, stdlib-first. Only dep: yaml.v3. Distroless static image; HTTP :8081 (TLS is the user's proxy), SW-P-08 :8910. Keep it light: no frameworks, no new deps without asking.
- Sode is the controller; **we are the router**. Single matrix (any matrix number is accepted and echoed), **extended** commands (16-bit src/dst), one level: **wire level 0 = video**. Other levels answer "nothing routed" (0xFFFF) and ignore connects. Sode must be set to `SODE_LEVEL_COUNT=1`, `SODE_LEVEL_WIRE_OFFSET=-1`.
- Videohub indices are 0-based and map straight to SW-P-08 dst/src numbers.
- Config: `/data/config.yaml` edited on the GUI; `VEP_*` env only seeds the file on first start. No PIN (LAN tool).
- Frontend: vanilla JS ES modules, no build step. Follow patchbay rules exactly (https://github.com/sethdotfm/patchbay llms.txt): `pb-` classes only, tokens only, no Bootstrap utilities, no border-radius, status always with text. Patchbay is vendored under `web/vendor/patchbay`; copy missing icons from upstream, don't fake them. It must look and feel like Sode.
- **Build DOM with `el()`, never `innerHTML`**: labels come from the hub.
- `internal/swp08` is copied from Sode (its `internal/` can't be imported). Fix bugs here, then tell the user so Sode can take them.

## Architecture

- `internal/state`: `Store` is the cache and the only shared state. The hub client is its only writer (`SetUp`, `SetDown`, `ApplyRoutes/Labels/Locks`, each returns/publishes only *real* changes); the SW-P-08 server and the web page `Subscribe` for events (a slow subscriber gets `EvResync` and re-reads `Snapshot`). `Router.Take(out, in)` is how a take goes to the hub.
- `internal/hub`: Videohub client. Text blocks: `HEADER:` line, `index value` lines, blank line ends; prelude ends with `END PRELUDE:`. Route with `VIDEO OUTPUT ROUTING:\n<out> <in>\n\n` → `ACK`/`NAK`. `PING:` keepalive, reconnect with backoff, periodic resync of labels/routes/locks.
- `internal/bridge`: SW-P-08 server, modelled on Sode's `swp08sim`. Rules:
  - ACK every valid frame at once, never wait on the hub (Sode's writer sends one message per ACK); NAK bad frames.
  - 0x82 connect → `Router.Take`; reply only via the hub's resulting route change as 0x84 to **all** clients. A no-op connect or a hub NAK sends nothing (like the Ultrix; Sode's take interrogates and times out honestly).
  - 0x81 → 0x83 from cache. 0x95 → 0x97, 64 per message. Names: std (0x64/0x66) and extended (0xE4/0xE6), cut to the asked length, folded to printable ASCII.
  - Locks L/O → protected by `protectDevice`. 0x93 → 0x94 dump (state in bits 4-5 of byte 1, device in the rest, verified against a real Ultrix). Changes pushed as 0x8D/0x8F, built to round-trip through `swp08.Parse`; Sode's periodic re-poll is the fallback. A connect to a locked output is not sent to the hub.
  - Hub down → close every session and refuse new ones until `END PRELUDE`. Sode reconnects and re-polls on its own.
- `internal/config`: `Defaults` → `FromEnv` (seed) → `Normalize`/`Validate`; YAML load/save.
- `internal/web` + `web/`: `/`, `/healthz`, `/api/state`, `/api/config`, `/events` (SSE, no websocket dep).
- `cmd/vhsim`: fake Videohub; **must keep mimicking real hardware quirks** (record them below as they are verified).

## SW-P-08 facts Sode relies on (verified against a real Ultrix)

- Extended connect (0x82) is confirmed by 0x84; routers send Connected only when a crosspoint *changes*, so a matching connect gets no reply.
- Extended tally dump 0x95 → 0x97 (64 per message); source 0xFFFF = nothing routed.
- Routers don't always announce changes; Sode interrogates before takes and re-polls.

## Verified against the real Videohub

(Empty until field testing. Record model, firmware, and each behaviour confirmed live: whether the routing block is echoed to the requesting client, ACK/NAK ordering, PING, `Device present`, takes to `L`-locked outputs, client limit.)
