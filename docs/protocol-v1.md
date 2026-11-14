# Relay protocol v1

All messages are JSON objects with a `type` field. Device and admin connections are authenticated with short-lived single-use WebSocket tickets issued over HTTP. The SQLite session table is shared by the two server processes.

## Device lifecycle

1. Admin provisions a device using `POST /api/admin/devices`; save the returned `deviceId` and `deviceToken` securely on the device.
2. Device requests `POST /api/device/ws-ticket` with `Authorization: Bearer <deviceToken>` and body `{"deviceId":"..."}`.
3. Device connects to `/ws/device?ticket=<one-time-ticket>`, then remains connected.
4. Device sends `{"type":"heartbeat","battery":80,"rttMs":30,"status":"ready"}` periodically; server replies `heartbeat-ack` and sends authorized admin clients `device-state` events.
5. Socket close marks device offline and emits `device-offline`.

## Admin lifecycle and access

Admin logs in over HTTP, obtains a websocket ticket from `POST /api/admin/ws-ticket`, then opens `/ws/admin?ticket=...`. The first message is a permission-filtered `device-list`. Online and state events are pushed only to admins/users with access to that device. The permission check is made server-side for every watch request and every event.

Access policy: admins have full access. A regular user sees a device only when they belong to a user group, the device belongs to a device group, and `group_access` grants `can_view`; management actions require `can_manage`.

## Watch session

Only one server-wide session is allowed. Admin sends `{"type":"watch","deviceId":"..."}`. Server checks authorization and online status, atomically reserves the session in SQLite, and sends the device `{"type":"watch-request","sessionId":"..."}`. The Android app must present the Android system screen-capture consent flow. Only after user approval should it start MediaProjection/WebRTC and reply `watch-ready`.

`offer`, `answer`, and `candidate` are relayed only between that viewer and the selected device. The server does not process screen frames. `watch-stop`, `watch-denied`, disconnection, or either peer closing ends the session. The Android client must stop capture on `watch-stop` and keep its device control connection online.

Use the existing Node protocol's explicit lifecycle style (`join`/`out`) when integrating legacy clients by mapping those messages to connect/disconnect events; do not treat arbitrary commands as trusted. This implementation deliberately does not define shell, keylogging, camera, or silent-control commands.
