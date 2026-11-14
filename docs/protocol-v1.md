# Relay protocol v1

All WebSocket messages are JSON objects with a `type` field. Device and admin connections are authenticated with short-lived single-use WebSocket tickets issued over HTTP. Both services must use the same local SQLite database. The relay stores connection state in memory and persists management metadata.

## Device lifecycle

1. Admin provisions a device using `POST /api/admin/devices`; save the returned `deviceId` and `deviceToken` securely on the device.
2. Device requests `POST /api/device/ws-ticket` with `Authorization: Bearer <deviceToken>` and body `{"deviceId":"..."}`.
3. Device connects to `/ws/device?ticket=<one-time-ticket>`, then remains connected.
4. Device sends `{"type":"heartbeat","battery":80,"rttMs":30,"status":"ready"}` periodically; server replies `heartbeat-ack` and sends authorized admin clients `device-state` events.
5. Socket close marks device offline and emits `device-offline`.
6. Admin device disconnect/disable sends the device a `disconnect` event and closes its socket. Device should stop media, mark the connection intentionally stopped, and not auto-reconnect until an admin re-enables it or the user explicitly reconnects.

## Admin lifecycle and access

Admin logs in over HTTP, obtains a websocket ticket from `POST /api/admin/ws-ticket`, then opens `/ws/admin?ticket=...`. The first message is a permission-filtered `device-list`. Online and state changes are pushed as `device-online`, `device-offline`, and `device-state` messages. The server checks authorization for every watch request and for each pushed device event.

Access policy: admins have full access. A regular user sees a device only when they belong to a user group, the device belongs to a device group, and `group_access` grants `can_view`; management actions require `can_manage`.

## Message routing and WebRTC

Each message must carry a valid `type`; all watch-scoped messages must carry the active `sessionId`. `watch` starts a single global view session, and the Android app must use explicit user action plus Android MediaProjection system consent before capture. The Android side creates the offer; `offer`, `answer`, and trickled `candidate` messages are relayed only between the authorized viewer and selected device. Queue candidates received before the remote SDP is set, and discard stale session IDs. The server relays signaling only and does not receive screen frames.

Exact message examples, the full sequence, cleanup rules, TURN setup, and a test checklist are in [`webrtc-client-flow.md`](webrtc-client-flow.md). Screen capture must be visibly consented to; this protocol does not support silent collection.

Use the existing Node protocol's explicit lifecycle style (`join`/`out`) when integrating legacy clients by mapping those messages to connect/disconnect events. Do not treat arbitrary commands as trusted.
