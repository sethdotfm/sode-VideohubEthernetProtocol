# Videohub Ethernet Protocol: verified findings

Primary source: Blackmagic Videohub Ethernet Protocol v2.3 (November 2023, Videohub 4.9.1),
<https://documents.blackmagicdesign.com/DeveloperManuals/VideohubEthernetProtocol.pdf>
(read in full, 8 pages). Landing page: <https://www.blackmagicdesign.com/developer/products/videohub/sdk-and-software>.
Cross-checks (search only, no extra detail found): Blackmagic forum thread <https://forum.blackmagicdesign.com/viewtopic.php?p=183643>
(asks what `END PRELUDE`, `ALARM STATUS` mean; no answer; "latest docs are 2.3"), gfto/videohubctrl (reports protocol 2.4 on a Micro Videohub),
Rust crate `videohub` (says real hubs send undocumented fields).

Transport: text over TCP 9990, one block = ALL-CAPS header ending in `:`, `index value` lines, blank line. Lines end with newline.
Clients must skip unknown blocks and unknown lines.

## (a) Is the routing block broadcast to the requester? ACK/NAK ordering
Doc: "When any route, label, or lock is changed ... by any client, the Videohub Server resends the applicable status block"
and the worked dialog shows `ACK` first and then the update block to the requester itself. So yes: the requester gets the
update too, after the ACK. Updates are asynchronous; clients must update their model only from status blocks.
Simultaneous changes can alter the result. Exact ordering on real hardware under load: UNCONFIRMED (verify on site).

## (b) PING
`PING:\n\n` is a no-op; the server answers `ACK\n\n` like any recognized command. Timeout/interval requirements: UNCONFIRMED.

## (c) Preamble
Order: `PROTOCOL PREAMBLE:` (`Version: 2.3`), `VIDEOHUB DEVICE:` (`Device present: true|false|needs _ update`,
`Model name`, `Video inputs`, `Video processing units`, `Video outputs`, `Video monitoring outputs`, `Serial ports`;
if not `true`, nothing further is sent until fixed), `INPUT LABELS`, `OUTPUT LABELS`, `MONITORING OUTPUT LABELS`,
`SERIAL PORT LABELS`, `VIDEO OUTPUT ROUTING`, `VIDEO MONITORING OUTPUT ROUTING`, `SERIAL PORT ROUTING`,
(`PROCESSING UNIT ROUTING`, `FRAME LABELS`, `FRAME BUFFER ROUTING` on Workgroup only), `VIDEO OUTPUT LOCKS`,
`MONITORING OUTPUT LOCKS`, `SERIAL PORT LOCKS`, (`PROCESSING UNIT LOCKS`, `FRAME BUFFER LOCKS`), `SERIAL PORT DIRECTIONS`,
and on Universal Videohubs `VIDEO INPUT STATUS`, `VIDEO OUTPUT STATUS`, `SERIAL PORT STATUS`.
Blocks for absent port types are omitted. Note: routing comes BEFORE locks in the real order (the task brief listed locks first).
A re-attached device resends everything except the preamble.
`END PRELUDE:` is NOT in v2.3. UNCONFIRMED from any primary source; newer firmware is reported (forum, our plan) to send it as a
terminator after the initial dump, along with `ALARM STATUS:`. The sim sends `END PRELUDE:\n\n` last. Clients must not require
blocks the doc lists; treat `END PRELUDE` as "initial dump complete" only when present.

## (d) Locks
Per-client view: `O` = locked by this client (same IP), `L` = locked by another client, `U` = unlocked. Lock: send `n O`.
Unlock own: `n U`. Force-unlock another's: `n F`; the resulting update shows `U`. Behaviour of a ROUTE request to an output
locked `L` (NAK vs ACK-and-ignore) and to one locked `O`: UNCONFIRMED (doc is silent; it only says an unperformable request may
yield an update that differs from the request). Sim: NAK for `L`, accepted for `O`. Routes to `O` presumably work: UNCONFIRMED.
Lock updates are per-client (each client sees O/L from its own viewpoint): inferred from the O/L definition.

## (e) Client limit
UNCONFIRMED (doc is silent). Sim: unlimited.

## (f) Requesting a block, partial updates
Client sends the header and a blank line (`OUTPUT LABELS:\n\n`); server replies `ACK`, then the full block. Updates carry only
changed items (doc examples: one lock line, two label lines). Whether a no-op route (same input) generates an update: UNCONFIRMED;
sim ACKs and sends nothing.

## (g) Labels
Encoding and max length: UNCONFIRMED (doc is silent). Label is everything after the first space of the line. Sim accepts any
text without newlines, UTF-8 passthrough, no length limit. Treat as untrusted, cut/fold on our side.
