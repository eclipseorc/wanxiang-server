# Relay protocol v1

All WebSocket messages are JSON objects with a `type` field. Device and admin connections are authenticated with short-lived single-use WebSocket tickets issued over HTTP. Both services must use the same local SQLite database. The relay stores connection state in memory and persists management metadata.

## Device lifecycle

1. The Android app generates a random `deviceId` locally on install, then requests `POST /api/device/ws-ticket` with body `{"deviceId":"...","model":"..."}` (model optional). The first request registers the device; later requests are idempotent.
2. The device receives a one-time `ticket` in the response.
3. Device connects to `/ws/device?ticket=<one-time-ticket>`, then remains connected.
4. Device sends `{"type":"heartbeat","battery":80,"rttMs":30,"status":"ready","ip":"192.168.1.100"}` periodically; server replies `heartbeat-ack` and sends authorized admin clients `device-state` events. The optional `ip` field is the device's Wi-Fi IPv4 address, used to correlate the device with its ADB connection for offline detection.
5. Socket close marks device offline and emits `device-offline`.
6. Admin device disconnect/disable sends the device a `disconnect` event and closes its socket. Device should stop media, mark the connection intentionally stopped, and not auto-reconnect until an admin re-enables it or the user explicitly reconnects.

## ADB offline detection (semi-automatic recovery)

The WebSocket relay periodically scans `adb devices` and, for devices that are business-online (heartbeat alive) but ADB-offline, sends the device a downstream `com` command:

```json
{"type":"com","command":"adb-status","data":{"connected":false,"ip":"192.168.1.100"}}
```

The device app should surface a notification guiding the user to re-enable wireless debugging. When the ADB connection is restored, a `connected:true` message is sent so the app can dismiss the notification.

## Admin lifecycle and access

Admin logs in over HTTP, obtains a websocket ticket from `POST /api/admin/ws-ticket`, then opens `/ws/admin?ticket=...`. The first message is a permission-filtered `device-list`. Online and state changes are pushed as `device-online`, `device-offline`, and `device-state` messages. The server checks authorization for each pushed device event.

Access policy: admins have full access. A regular user sees a device only when they belong to a user group, the device belongs to a device group, and `group_access` grants `can_view`; management actions require `can_manage`.

## Remote control

Remote screen control is handled by a separate `scrcpy-server` process (`:9004`) rather than the WebSocket relay. It bridges H.264 video and control events between the browser and a device over ADB; pairing/connection are HTTP endpoints on the admin server, and the viewing session is a dedicated WebSocket. See the scrcpy section of `README.md` for details.

Use the existing Node protocol's explicit lifecycle style (`join`/`out`) when integrating legacy clients by mapping those messages to connect/disconnect events. Do not treat arbitrary commands as trusted.
