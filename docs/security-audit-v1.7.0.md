# mtroamd v1.7.0 — pre-release security audit (2026-06-15)

Deep multi-agent audit (6 dimensions, every finding adversarially verified +
completeness critic) of the **delta since v1.6.2** (first-attach-wins / exclusive-if-free,
client-identity handover, idle-gated kill-and-resume recover, wedge ResetWedge, fg
anchors / kill-fg) plus the always-critical surface. **25 findings — 1 high, 4 medium,
20 low, 0 refuted** + 6 completeness-critic interaction gaps.

Threat model: a client reaches the daemon only past the cert-pinned, SSH-bootstrapped
transport — but treat the attach/session layer as if a buggy or hostile client can send
well-formed/malformed frames, race attaches, spoof fields, and disconnect abruptly. PTY/
kill paths run with the user's own privileges. The exposure is therefore primarily
**data-loss / takeover within the user's own trust domain**, not remote-unauth RCE.

---

## The release blocker: the recover + displacement cluster

These interact — the critic's interaction chain (#3) is the load-bearing insight: the
new exclusive-displacement + recover + kill-fg primitives compose into a takeover that
ends in a **real SIGTERM to a stolen session's foreground group**.

**H1 (high) — recover idle-gate can SIGTERM a silent foreground tool → repo corruption.**
`recover.go:101-147,268-300`. The gate's safety premise is "agents animate a spinner the
whole time a tool runs, so PTY-quiet ⇒ nothing running", backed only by a *secondary*
fg-equality check against a **timestamp-less, ≤5s-stale** cached fg (`conn.go:242-245`,
`sidecar.go:534,556-583`). A tool that goes quiet >1.5s while foreground (git-commit→\$EDITOR
handoff, a lock wait, buffered output) passes both the quiescence gate and the output-only
pre-kill re-confirm → `kill(-pgid, SIGTERM)` lands mid-write. **Fix:** stamp fg freshness
and abort-as-busy if stale; **re-read the LIVE foreground pgid immediately before SIGTERM
and refuse unless its comm is still the agent**; drop the spinner assumption from the doc.

**M1 (med) — recover/kill mislabels ANY foreground as "the agent".** `recover.go:240,262,293`.
No agent allowlist — `/exit` + `claude --continue` + SIGTERM fire against whatever is
foreground. **Fix:** gate `runRecover` on a known-agent allowlist (claude/codex/…); refuse
(RecoverStageError) when `agent==""` or unrecognized.

**M2 (med) — recover is reachable by any well-formed client.** `pumps.go:198-222`,
`session.go:977-1028`. Recover is gated only by `mode==AttachExclusive`, and a plain
exclusive attach **unconditionally displaces** the incumbent → any client can seize control
and force-restart (or, per critic #3, SIGTERM) the user's agent. **Fix:** bind the
destructive recover to the established owner ClientID (refuse from a non-owner), and/or
rate-limit + audit-log recover with the requesting ClientID.

**M3 (med) — displaced exclusive client can still inject stdin/Recover during the
displacement window.** `pumps.go:100-135,198-222`; `session.go:948-964`. The stdin/Recover
authorization is a goroutine-local `mode` captured at attach; `CancelRead` only aborts a
*blocked* read, so an in-flight frame is still written to the PTY the **new** owner now
controls — and an in-flight Recover runs on a `context.Background()` (`session.go:347-357`)
that survives the displaced client's teardown. **Fix:** make WriteStdin/Resize/recover a
**live ownership check** under `s.mu` (verify the caller's attach `gen` is still the current
exclusive holder) before acting.

**M4 (med) — two rapid Recover frames race the single unkeyed `PTYByteObserver` slot.**
`recover.go:109-117`, `session.go:694-698`. A superseded recover's deferred
`SetPTYByteObserver(nil)` clears the live recover's observer → its idle gate sees a
permanently "quiet" PTY → premature kill. **Fix:** identity-key the observer slot
(generation token, mirror `recoverGen` at `session.go:368-375`). (Critic #5: the same
superseded-cleanup pattern lets a stale `ResetWedge` double-close `pendingTimerCh` vs the
live `ArmResize` — fix together.)

**Critic interaction gaps to fold in:** #1 `observeForegroundAnchor` is only wired from
Pump, not the attach/notify paths its docstring promises → stale anchors + feeds H1's
freshness problem; #2 AttachAck reads `Fg` once but `FgSince`/`FgSinceSeq`/`Cwd` separately
→ a torn `Cwd` drives a wrong-directory `cd && claude --continue`; #6 the new
`displacedCloseLinger`(250ms)+backstop(500ms) per displacement is a cheap goroutine-pin
amplifier under repeated displacement.

---

## Medium/low hardening (cheap, recommended with the cluster)

- **L (D3) — ClientID + all wire strings unbounded.** `protocol.go:252,43`. Add a ClientID
  length cap (~128B) in `resolveAttach` and set `MaxByteStringLength`/`MaxStringLength` on
  `StrictDecMode` so the decoder is self-bounding (not reliant on the frame cap).
- **L (D2) — ClientID match is non-constant-time.** `session.go:985`. Use
  `subtle.ConstantTimeCompare` (parity with the SID compare at `protocol_handler.go:641`).
- **L (D3/D5) — `SavePrompt` decoded but unused** (`recover.go:206`, `protocol.go:490-493`).
  Remove it, or document "reserved/never injected" + a test asserting it never reaches
  WriteStdin (latent command-injection footgun if ever wired in).

## Defer (documented, not blocking)

DoS-aggregation hardening for genuinely-public binds (rate-limit IPv6 /64 aggregation,
table-exhaustion two-tier cap — `ratelimit.go:150-197`; TCP-on-unspecified warn→fail-closed
— `tailnet.go:125-141`); IPC belt-and-suspenders (SO_PEERCRED, umask-around-bind,
XDG_RUNTIME_DIR VerifyParentDir on the server bind — `ipc/server.go`, `serve.go:173`);
TOCTOU best-effort tightening on `killForegroundGroup`/`/proc` reads (`fg_kill.go:32`,
`fg_linux.go:51`); cert renewal overlap window (`cert.go:147`); `foregroundCwd` shell-quoting
(latent, the `cd` form isn't shipped yet — `fg_linux.go:91`); SessionSearch MaxMatches upper
cap (`buffer_search.go:59`). All are within the same-uid / public-bind-only / not-yet-wired
envelope — real, but not a v1.7.0 ship blocker.

---

## Fix status (2026-06-15)

**DONE (committed on develop, build+vet clean):**
- **H1** — kill path re-reads the LIVE foreground and refuses the SIGTERM unless it
  still matches the agent (`fg_kill.go`, threaded through KillForeground→Conn.KillFg→
  FrameKillFg→sidecar). Residual (child-in-agent-pgid) documented.
- **M1** — recover refuses unless the foreground is a recoverable agent (claude).
- **M2/M3** — `Session.IsCurrentExclusive(gen)` gates stdin/Resize/Recover LIVE, so a
  displaced client can't inject or fire recover at the new owner's session.

H1 also de-risks **M4** in practice: a premature-idle race can no longer drive a kill
of a non-agent (the live-fg refusal catches it).

**REMAINING (lower-severity hardening, before tag):**
- **M4** — identity-key the `PTYByteObserver` slot (+ ResetWedge) so a superseded
  recover's cleanup can't clear the live recover's observer. (Now largely mitigated by
  H1; still worth the keyed slot for correctness.)
- **Critic #1** — wire `observeForegroundAnchor` from the attach/notify paths (stale
  anchors on a silent fg transition; feeds the iOS age/size banner).
- **Critic #2** — single-read `Fg`/`FgSince`/`FgSinceSeq`/`Cwd` in AttachAck/AgentNotify
  so a torn `Cwd` can't drive a wrong-directory restart.
- Cheap lows: explicit ClientID length cap; constant-time ClientID compare (marginal —
  ClientID is a non-secret hint); document/guard the unused `SavePrompt`.

## Recommended fix-before-tag set

**Block the v1.7.0 tag on the recover/ownership cluster:** H1 (live-fg recheck before kill +
freshness gate) + M1 (agent allowlist) + M2 (recover owner-bound) + M3 (live ownership check
on stdin/recover) + M4 (keyed observer slot + ResetWedge) + critic #1/#2 (anchor wiring +
AttachAck single-read). Plus the three cheap L hardening items (length bounds, constant-time
ClientID, SavePrompt). Defer the rest with this doc as the record.

Rationale: these are all the v1.7.0 *new* surface, several CONFIRMED, and they compose into a
data-loss/takeover chain — exactly what a pre-release security gate should stop before the
stable tag ships fleet-wide via the channel pin.
