# WebRTC screen-view flow (protocol v1)

This describes the exact end-to-end flow used by the Go relay and the browser console. The relay is a signaling router only: the video is carried by WebRTC between Android and the browser (or via a configured TURN relay), not through the Go WebSocket server.

## Preconditions

- The device has already registered and has a long-lived authenticated `/ws/device` connection.
- The admin has logged in, has permission to view the device, fetched a one-use WS ticket, and connected to `/ws/admin`.
- Use HTTPS/WSS in deployed environments. The demo STUN URL is for connectivity discovery, not a TURN service and not a production availability promise.
- The Android app must obtain fresh, explicit Android MediaProjection consent as required by the Android version. The device user should see an in-app/notification prompt describing who requested the session. A denied or timed-out request must not capture the screen.

## Message sequence

### 1. Admin requests a session

Admin -> relay:

```json
{"type":"watch","deviceId":"<device-id>"}
```

The relay checks the current user's `can_view` permission, verifies the device is online, reserves the single global watch slot, creates an unguessable `sessionId`, then sends:

Admin <- relay:

```json
{"type":"watch-started","sessionId":"<session-id>","deviceId":"<device-id>"}
```

Device <- relay:

```json
{"type":"watch-request","sessionId":"<session-id>","requestedBy":"<user-id>"}
```

The Android app must validate that it is not already handling another session, show the user an actionable prompt, and start the system capture-consent flow only after user action. The service must not infer consent from the WebSocket request.

### 2. Device authorizes or declines

If the user declines (or the request expires), Device -> relay:

```json
{"type":"watch-denied","sessionId":"<session-id>","reason":"user-denied"}
```

The relay forwards this only to the session's viewer and ends the session.

After system consent succeeds, Android creates a `PeerConnection`, attaches a video track backed by `MediaProjection`, and sends:

```json
{"type":"watch-ready","sessionId":"<session-id>"}
```

`watch-ready` is informational; do not send it before MediaProjection consent and successful local capture setup. If setup fails, send `watch-denied` with a non-sensitive reason and release acquired resources.

### 3. Offer/answer negotiation

Android is the offerer. Device -> relay -> admin:

```json
{"type":"offer","sessionId":"<session-id>","sdp":"<SessionDescription.description>"}
```

The browser sets the remote offer, creates an answer, sets its local description, then sends Admin -> relay -> Device:

```json
{"type":"answer","sessionId":"<session-id>","sdp":"<SessionDescription.description>"}
```

The Android app sets that as the remote answer. Send SDP exactly as produced by the WebRTC library; JSON-escape it with the normal JSON encoder rather than hand-escaping newlines.

### 4. Trickle ICE

Both sides may emit candidates before or after receiving the answer. Each side forwards each candidate through the same authenticated signaling WebSocket, including the session ID:

```json
{
  "type":"candidate",
  "sessionId":"<session-id>",
  "candidate":"candidate:...",
  "sdpMid":"0",
  "sdpMLineIndex":0
}
```

The receiver calls `addIceCandidate`. If its remote description has not yet been set, queue the candidate and add it after `setRemoteDescription` succeeds. Ignore candidates whose `sessionId` does not match the currently active watch session. This avoids losing early candidates and prevents a late candidate from a previous watch being applied to the next session.

### 5. End/cleanup

Admin -> relay:

```json
{"type":"watch-stop","sessionId":"<session-id>"}
```

The relay sends Device `watch-stop` with the session ID, sends Admin `watch-ended`, and releases the global watch slot. The Android app must stop the capturer, dispose the peer connection and tracks, stop the MediaProjection foreground service, and send `watch-stopped` if it initiated the stop:

```json
{"type":"watch-stopped","sessionId":"<session-id>"}
```

Decline, device disconnect, admin disconnect, disabled device, and ICE failure should also terminate the media session and release resources. Keep the device's long-lived control WebSocket online after a normal watch ends. Do not send `out` for a watch stop.

## ICE server configuration

The checked-in dashboard currently uses a public STUN server. STUN helps peers discover candidate addresses but does not relay media and cannot guarantee connectivity through restrictive NAT/firewalls. For reliable enterprise networks, deploy or procure TURN, configure both clients with the same ICE server credentials, and use short-lived TURN credentials. Example shape (replace placeholders; do not commit secrets):

```json
{"iceServers":[
  {"urls":["stun:stun.example.net:3478"]},
  {"urls":["turn:turn.example.net:3478?transport=udp","turns:turn.example.net:5349?transport=tcp"],"username":"<ephemeral-user>","credential":"<ephemeral-secret>"}
]}
```

The relay does not make TURN credentials or choose ICE servers yet; configure the browser and Android client through a trusted deployment configuration. Never use a permanent TURN secret in browser code.

## Test checklist

1. Start HTTP and WebSocket services with the same local `DB_PATH`, set `WS_ALLOWED_ORIGINS` to the exact console origin, and open the console over HTTP locally or HTTPS in a deployed test.
2. Provision a device through the console, save its device ID/token, and connect the Android app to the device WebSocket using the ticket flow documented in `protocol-v1.md`.
3. Confirm the console receives `device-online` and subsequent `device-state` events.
4. Click “查看屏幕”. Android must show a visible request and the OS capture-consent prompt; decline once and verify no stream is created.
5. Request again, accept consent, verify offer/answer and candidates include the same session ID, then check browser video and logs.
6. Stop from the browser and confirm Android capture stops while the device remains online. Repeat by stopping on-device and by closing either peer.
7. Test a network requiring TURN. A STUN-only setup may connect on a LAN but is not evidence that remote enterprise networks will work.

## Limitations

The current relay allows one global watch session, one relay process, and SQLite on one host. It does not record screen video, implement remote touch input, or guarantee reliable ICE connectivity. Add TURN configuration and multi-node signaling/routing before broader deployment.
