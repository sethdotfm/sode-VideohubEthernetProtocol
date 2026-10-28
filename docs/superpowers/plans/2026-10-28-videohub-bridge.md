# Plan: sode-vep — Videohub ↔ SW-P-08 bridge for Sode (MVP)

## Context

Sode (`~/Desktop/sode`) is an SW-P-08 **controller**. It connects out over TCP to one router, which today is a Ross Ultrix. Blackmagic Videohubs speak the Videohub Ethernet Protocol (text over TCP 9990), not SW-P-08. This bridge sits between them:

- To Sode it **is the router**: it listens for SW-P-08 the way `cmd/swp08sim` does.
- To the Videohub it **is a client**: it connects to the hub's IP on port 9990, keeps names, routes and locks cached, and passes takes through.

It is a very light distroless Go container with a one-page GUI on **:8081** that looks identical to Sode, because it uses the same vendored patchbay CSS and the same code style. Repo: `git@github.com:sethdotfm/sode-VideohubEthernetProtocol.git`. It is empty and public. The local directory `~/Desktop/sode-vep` is not a git repo yet.

**Decisions (the user's answers):**
- Routing is two-way.
- Video routing only, on a single level (wire level 0).
- The SW-P-08 code is copied from sode; sode itself is untouched.
- Videohub output locks (L/O) are reported as SW-P-08 protects.
- The GUI shows status, settings (saved to `/data/config.yaml`) and a read-only live table.
- Videohub down = router down for Sode.
- No PIN.

**Approach:** there is one sensible way to build this, which is to mirror sode's own stack: Go, stdlib-first, distroless. Using the sim's server loop as the base means every reply shape is one Sode already accepts. Node or Python was ruled out: it would break the "identical look and feel" goal and the house rules.

## Layout (new repo)

```
CLAUDE.md                  compact, derived from sode's (task 1)
README.md                  settings table, Sode-side settings, compose usage
go.mod                     github.com/sethdotfm/sode-vep; stdlib + yaml.v3 (+ coder/websocket only if the GUI needs push; prefer SSE/polling → no ws dep)
cmd/vep/main.go            flags/env, -healthcheck, wiring
cmd/vhsim/main.go          fake Videohub on :9990 (mimics real quirks)
internal/swp08/            copied frame.go, msg.go + tests from sode (+ server-side builders added here: ExtProtectTally etc.)
internal/hub/              Videohub client: block parser, session, reconnect, PING keepalive, command queue
internal/state/            shared cache: Store interface (names, routes, locks, up/down, subscribers)
internal/bridge/           SW-P-08 server (based on swp08sim serve/handle), reads Store, sends takes to hub
internal/config/           Defaults → env seed → YAML /data/config.yaml, Validate
internal/web/              HTTP :8081: /, /healthz, /api/state, /api/config, /events (SSE)
web/                       index.html, app.js, app.css, vendor/patchbay (copied), go:embed
Dockerfile, compose.yaml   distroless; compose profiles: sim (vhsim), sode (real sode image)
docs/superpowers/specs/    design doc (the approved version of this plan)
```

## Contracts (define these in the skeleton commit so tasks can run in parallel)

**`state.Store`** is shared by the hub client and the bridge, and its mutex is the only lock:
- Fields: `Up bool`, `Model`, `Inputs []string`, `Outputs []string`, `Route []int` (out→in, -1 = unknown), `Lock []byte` (`U`/`O`/`L`).
- Writes come from the hub only: `SetPrelude`, `ApplyRoutes`, `ApplyLabels`, `ApplyLocks`, `SetDown`. Each returns the diffs.
- Subscribers get typed change events (route changed, names changed, lock changed, up/down). The bridge and the web page both subscribe.
- Takes go out through an interface `Router.Take(out, in int) error`, implemented by the hub client.

**SW-P-08 server rules** (the sim's `handle()` is the template):
- ACK every valid frame right away. Never wait on the hub, because Sode's writer sends one message per ACK. Bad frames get a NAK.
- **Matrix:** accept any matrix and echo it back, as the sim does. **Level:** only wire level 0 is real. For other levels, interrogates and dumps get empty answers (src 0xFFFF) and connects are ignored. README tells users to set Sode to `SODE_LEVEL_COUNT=1`, `SODE_LEVEL_WIRE_OFFSET=-1`.
- **0x82 connect** → `Router.Take`. Nothing is sent to Sode directly. A **0x84** goes to *all* SW-P-08 clients only when a hub routing block differs from the cache. A no-op connect gets no reply, matching the Ultrix quirk, and Sode's take interrogates. A hub NAK also sends nothing, so Sode's timeout and interrogate report the failure honestly.
- **0x81 interrogate** → 0x83 from the cache. **0x95 tally dump** → 0x97, 64 per message, using sode's `ExtTallyDump`.
- **Names:** support std (0x64/0x66) and extended (0xE4/0xE6) requests with `NamesResp`. Truncate or pad to the requested length code, and fold UTF-8 down to printable ASCII.
- **Protects:**
  - 0x93 dump → 0x94 in the format verified in sode's CLAUDE.md: state in bits 4-5 of byte 1, device in the rest.
  - Lock L/O → protected by a fixed device number (configurable, default 1).
  - Lock changes are pushed as 0x8D/0x8F, built byte-for-byte to match sode `Parse`. Sode has never seen these live, so a test round-trips them through the copied `Parse`. Sode's periodic protect re-poll is the fallback.
  - A connect to a locked output is not sent to the hub.
- **Hub down:** close every SW-P-08 session, and refuse new ones (accept, then close at once) until the hub's `END PRELUDE` arrives. When the hub comes back up, accept again. Sode reconnects by itself and re-polls.

**Videohub client:**
- Connect, then parse the prelude blocks up to `END PRELUDE`.
- Blocks are a `HEADER:` line, then `index value` lines, ended by a blank line, with 0-based indices.
- Send `VIDEO OUTPUT ROUTING:\n<out> <in>\n\n` and expect `ACK`/`NAK`.
- Send `PING:\n\n` every ~10s, and drop and reconnect when no reply comes.
- Resync every 60s (configurable) by requesting the label, routing and lock blocks. This is the "poll names and status" part.
- Watch for `Device present: false`.
- Reconnect with backoff.

**Facts to check against Blackmagic's Videohub protocol doc before building the hub client (task 2):**
- Is the routing block echoed back to the client that made the change?
- How do ACK/NAK and the broadcast interleave?
- How does PING behave?
- What does `Device present` look like?
- What happens on a take to an output locked `L`?
- What is the client limit?

Record the answers in CLAUDE.md, and build vhsim to mimic them.

## Execution (subagent-driven; Sonnet subagents; review per task + final)

Git setup comes first:
1. Clone the empty remote into `~/Desktop/sode-vep`, or `git init` there and add the remote.
2. Commit the skeleton on `main`.
3. Push to `origin`. This is approved as part of this plan.
4. Each later task runs in its own git worktree on its own branch. A reviewer subagent (code review) reviews each branch; I fix what it finds, merge to `main`, push, and remove the worktree.

Commits use the Co-Authored-By line.

**Preference (the user asked for this; do it first after approval):** use **AGENTS.md** and not CLAUDE.md for project guidance. Create `~/.claude/CLAUDE.md`, which is the global Claude Code instructions file and doesn't exist yet, containing: "Write project guidance to AGENTS.md, not CLAUDE.md. If a repo needs CLAUDE.md for Claude Code to load it, make CLAUDE.md a one-line `@AGENTS.md` import." Also save this as a feedback memory. In this repo the compact guidance goes in `AGENTS.md`, and `CLAUDE.md` contains only `@AGENTS.md` so Claude Code still loads it. Every "CLAUDE.md" below means AGENTS.md.

| # | Task | Notes |
|---|------|-------|
| 0 | Write the spec to `docs/superpowers/specs/2026-10-28-videohub-bridge-design.md` and the plan to `docs/superpowers/plans/` | from this file |
| 1 | **Compact CLAUDE.md** plus skeleton: go.mod, copied `internal/swp08` with passing tests, copied patchbay vendor, `state.Store` with its interfaces and tests, `config`, stub `cmd/vep` | done by me; commit to main and push |
| 2 | Protocol verification, then **`cmd/vhsim`** | subagent fetches the Videohub protocol doc; sim env: `VHSIM_INPUTS/OUTPUTS`, `VHSIM_PANEL_SECONDS`, `VHSIM_LOCKS` |
| 3 | **`internal/hub`** client | parser + session tests against vhsim |
| 4 | **`internal/bridge`** SW-P-08 server | tests use the copied `swp08.Client` against a fake `Store`/`Router` |
| 5 | **Web GUI** `internal/web` + `web/` | patchbay rules exactly: `pb-` classes, tokens, no radius, status with text, `el()` and never `innerHTML`; SSE for live updates (no ws dep) |
| 6 | **Docker/compose** + README | distroless static nonroot, `-healthcheck`; compose profiles `sim` and `sode` (builds `../sode` or uses `ghcr.io/sethdotfm/sode`) |
| 7 | Integration + final review | see Verification |

Tasks 2 and 3 run in sequence. Tasks 4 and 5 can run in parallel with them, because they only depend on the Store contract.

**Compact CLAUDE.md (task 1)** keeps from sode:
- the commands section, adapted;
- stdlib-first and no new deps without asking;
- distroless;
- the patchbay rules plus `el()`/no `innerHTML`;
- the config pattern (env seeds the YAML on first start);
- "the sim must mimic real hardware quirks";
- the SW-P-08 facts verified on the Ultrix that Sode relies on: extended commands only, no Connected on no-op connects, the protect dump format.

It adds the bridge architecture, the contracts above, the Sode-side settings, and an empty **"Verified against the real Videohub"** section to fill during field testing. It leaves out everything about history, ext, follows, the PIN and keyboard handling.

## Verification (the gate before "ready for real-world testing")

1. Run `go test ./... && go vet ./...` in the Docker build, as sode does.
2. Run a Go integration test with vhsim, the bridge and the copied `swp08.Client`. It covers:
   - a take reaching the hub, with 0x84 broadcast;
   - a route made in vhsim reaching Sode as 0x84;
   - a no-op take getting no reply, with the interrogate correct;
   - lock → protect in both the dump and the change message;
   - a take to a locked output not reaching the hub;
   - hub down → sessions closed and refused, then recovery;
   - names truncated and folded to ASCII.
3. Run `docker compose --profile sim --profile sode up --build`: the real Sode on :8080 points at the bridge, the bridge points at vhsim, and the GUI is on :8081. Check by hand, or with a curl or ws script:
   - names load in Sode;
   - a take from Sode changes vhsim;
   - a vhsim panel route shows up in Sode;
   - a locked output shows as protected;
   - stopping vhsim shows the router down in Sode and down in the GUI.
4. Run a final code review across the whole repo, then push `main`. Hand over the README instructions for pointing it at the real Videohub IP. The "Verified against the real Videohub" checklist is what to confirm on site.
