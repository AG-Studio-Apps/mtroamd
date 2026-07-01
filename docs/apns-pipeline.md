# APNs-from-mtroamd push pipeline (design)

Status: design sketch. Not yet built. Owner: James.

How mtRoamd notifies backgrounded iOS clients (meshTerm, harness) about session
events without the phone holding a live socket. The daemon already holds the real
mtRoam session server-side, so the iOS problem is not "keep a connection alive"
(impossible for a normal app on iOS) but "wake the phone and show liveness". On
iOS the only sanctioned background-wake path is Apple Push Notification service
(APNs), so this pipeline is how a daemon event becomes a push.

See also: `mtroam-protocol.md` (the app<->daemon wire protocol that carries token
registration), and the iOS-side island-keepalive / widget-surfaces work.

## The load-bearing constraint: you cannot push directly to APNs from the daemon

APNs scopes every push to a *topic* equal to the app's bundle ID
(`com.betchley.meshterm`), and only a signing key belonging to the Apple Team that
owns that bundle ID may push to that topic. A user's self-hosted mtRoamd has no
such key, and we cannot ship our Team's `.p8` inside an open-source daemon that
runs on every user's box: that would leak a Team-level push credential (anyone
could push to any meshTerm device) and Apple would revoke it.

Therefore the architecture is forced to include exactly one thin central piece: a
**push relay** that holds the APNs key and is the only thing that talks to APNs.
This contradicts the "daemon owns everything, zero cloud" ethos, so the relay is
designed to be minimal, stateless, and blind (see below). Everything else keeps
the daemon-owns-the-session moat intact.

## Topology

```
  iOS app                    mtRoamd                  Push Relay              APNs
 (meshTerm/                (user's box,             (we operate,           (Apple)
  harness)                  per-user)                stateless, blind)
     |                          |                         |                    |
     | 1. register w/ APNs      |                         |                    |
     |<---- device token --------------------------------------------- (from OS)
     |                          |                         |                    |
     | 2. App Attest ------------------------------------>|                    |
     |<---- short-lived push-grant -----------------------|                    |
     |                          |                         |                    |
     | 3. subscribe(sessionID,  |                         |                    |
     |    deviceToken,          |                         |                    |
     |    activityToken,        |                         |                    |
     |    push-grant)           |                         |                    |
     |------------------------->| stores per-session subs |                    |
     |                          |                         |                    |
     |            === app backgrounds / drops socket ===  |                    |
     |                          |                         |                    |
     |                          | 4. event fires          |                    |
     |                          |  (agent done /          |                    |
     |                          |   approval / output)    |                    |
     |                          |--- POST {token,payload, |                    |
     |                          |      grant} ----------->| 5. verify grant    |
     |                          |                         |  sign JWT, forward |
     |                          |                         |---- HTTP/2 push -->|
     |<------------------- 6. delivered to device <---------------------------|
     |  (alert / wake / Live Activity update)             |                    |
```

## Components

### 1. Token acquisition (iOS)

- `registerForRemoteNotifications()` yields the per-install **device token**.
- Each Live Activity yields its own **activity push token**
  (`activity.pushTokenUpdates`).
- iOS 17.2+ yields a **push-to-start token** for starting an Activity remotely.

The app forwards all of these to the daemon over the existing mtRoam wire
protocol, which is already an authenticated channel between the app and the
user's own daemon. No new trust boundary is introduced app<->daemon.

### 2. Daemon subscription store (`pushmgr`)

New mtRoamd component. Keeps:

```
sessionID -> [{ deviceToken, activityToken, grant, grantExpiry }]
```

Watches the session state the daemon already tracks (foreground kind, agent turn
state, output-since-detach, process exit, takeover) and turns state transitions
into push intents. Owns coalescing and rate-limiting so we stay inside APNs
budgets. When the daemon is authoritative for a session it is the natural place
to decide "this event is worth a push".

### 3. The relay (the one central piece)

A tiny stateless HTTPS service:

- Holds the APNs `.p8` key, Key ID, Team ID. Mints and reuses an ES256 JWT
  (refresh under 1h, reuse over 20min). Keeps long-lived HTTP/2 connections to
  `api.push.apple.com` (`api.sandbox.push.apple.com` for dev builds).
- **Stores nothing.** Receives `{deviceToken, apns-topic, apns-push-type,
  payload, push-grant}`, verifies the grant, signs, forwards, returns the APNs
  status. No user database, no who-notified-whom logs.
- **Abuse resistance without a user account:** the *device* proves it is a
  genuine instance of the app via **App Attest / DeviceCheck**, obtains a
  short-lived push-grant from the relay, and hands that grant to its daemon. The
  daemon presents the grant when pushing. Randoms cannot spam users' devices and
  the relay still holds no long-term identity.

The relay should itself be open-source and minimal, so "the cloud piece" is
auditable and clearly a blind stamp-and-forward rather than a backend.

### 4. Payload types

Alert (user-visible, survives force-quit) - highest value, ties to Agent mode:

```
:method  POST /3/device/<deviceToken>
apns-topic         com.betchley.meshterm
apns-push-type     alert
apns-priority      10
apns-collapse-id   sess-<id>-agentdone      # coalesces repeats
{ "aps": { "alert": {"title":"Claude finished","body":"~/project - 3 files changed"},
           "sound":"default", "interruption-level":"time-sensitive" },
  "sess": "<id>" }
```

Silent / background (wake to reconnect+prefetch; NOT delivered if force-quit,
tight budget, unreliable timing):

```
apns-push-type  background
apns-priority   5
{ "aps": { "content-available": 1 }, "sess":"<id>", "reason":"output" }
```

Live Activity update (Dynamic Island liveness, "distro glyph is liveness"):

```
apns-topic       com.betchley.meshterm.push-type.liveactivity
apns-push-type   liveactivity
{ "aps": { "timestamp": <unix>, "event":"update",
           "content-state": { "distro":"debian", "agent":"running", "lastLine":"..." },
           "stale-date": <unix+N>, "dismissal-date": <unix+M> } }
```

`event:"start"` against the push-to-start token spins up an Activity remotely
when an agent turn begins.

JWT (APNs token auth): ES256 over the `.p8`, header `{alg:ES256, kid:<KeyID>}`,
claims `{iss:<TeamID>, iat:<now>}`. Reuse one JWT across many pushes; refresh
inside the 20min..1h window.

Topics: alert/background use `com.betchley.meshterm`; liveactivity uses
`com.betchley.meshterm.push-type.liveactivity`.

### 5. Payload privacy hardening

The relay and Apple see whatever text is in an alert. For sensitive content, send
a generic alert ("Session update") plus an encrypted blob, and decrypt it in a
**Notification Service Extension** on the device using a key the app already
holds. Silent and liveactivity payloads carry only opaque IDs. Net: the relay
stays a blind stamp-and-forward even for alerts.

## Failure modes to design for now

- **Token rotation:** APNs `410 Unregistered` -> daemon prunes that token from
  the session subscription.
- **Force-quit reality:** only *alert* pushes render; silent and liveactivity are
  dropped. Critical events (approval needed, session died) must be alerts, not
  silent nudges.
- **Budgets:** silent push is a few-per-hour budget with unreliable timing; never
  use it as a heartbeat. Live Activity updates are budgeted too (raise the
  ceiling with `NSSupportsLiveActivitiesFrequentUpdates`, still finite).
- **Graceful degradation:** the relay is on the *notify* path, never the *data*
  path. If it is down the app still works normally on foreground reconnect. The
  central piece is never load-bearing for core function.

## Optional no-relay adjunct: Local Push Connectivity

For homelab users whose phone is often on the same LAN as their daemon,
`NEAppPushProvider` (Local Push Connectivity) lets the app hold a persistent
connection to the *local* daemon over that WiFi and deliver pushes with **no APNs
and no relay at all**. Caveats: needs the `app-push-provider` Network Extension
entitlement (Apple approval, historically restrictive), only works on the
designated WiFi, and it is another Network Extension process. Not a general
answer, but a genuine "when home, zero-cloud" optimization that fits the
self-hosted crowd unusually well. Layer on top of APNs later.

## Suggested phasing

- **Phase 0** - token plumbing app->daemon + `pushmgr` skeleton that logs "would
  push". No relay, no Apple dependency. Buildable and testable on the Linux box
  (all Go daemon + protocol).
- **Phase 1** - minimal relay + App Attest grant + **alert pushes** for
  agent-complete / approval-needed. Lands the Agent-mode UX.
- **Phase 2** - **Live Activity update pushes** feeding the Dynamic Island (folds
  into island-keepalive).
- **Phase 3** - silent-push reconnect nudges + Live Activity remote-start.
- **Phase 4 (optional)** - Local Push Connectivity for on-LAN, zero-relay
  delivery.

## Why not just run in the background instead

iOS gives a normal (non-VPN) app no way to hold a live socket open in the
background. The only `UIBackgroundModes` that grant continuous execution
(`audio`, `location`, `voip`) require a genuine reviewable use of that capability
and are App Store rejections as keepalive pretexts. A `NEPacketTunnelProvider`
(VPN) does get a persistent own-process lifecycle, but iOS allows only one active
packet-tunnel VPN at a time, so shipping one would evict the user's real Tailscale
app (and any corporate VPN) whenever meshTerm's tunnel is up - a non-starter for a
Tailscale-native product. Push + daemon-held session is the messaging-app model
(WhatsApp/Signal/iMessage do not hold background sockets either) and is the
correct spine here.
