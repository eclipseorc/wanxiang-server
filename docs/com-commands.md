# WebSocket application commands

Application commands use a common JSON envelope with `type: "com"`. Command names are explicit and unknown commands are rejected; do not treat command strings as executable actions.

## Ping / pong

The management dashboard sends an application-level ping approximately every 25 seconds:

```json
{"type":"com","command":"ping","requestId":"ping-1731670000000-1","data":{"sentAt":1731670000000}}
```

The relay responds on the same connection, preserving the request ID and timestamp:

```json
{"type":"com","command":"pong","requestId":"ping-1731670000000-1","data":{"sentAt":1731670000000}}
```

The dashboard matches responses by `requestId`, calculates round-trip time, and reports a timeout if no response arrives within 10 seconds. This application-level command is separate from WebSocket protocol Ping/Pong control frames, which remain enabled for connection keepalive.

Malformed and unsupported commands receive a correlated error response:

```json
{"type":"com","command":"error","requestId":"request-id","data":{"code":"unsupported_command","message":"unsupported command"}}
```

The relay also continues to accept the legacy admin message `{"type":"ping","sentAt":...}` during migration. New clients should use the `com` envelope.

## Extending commands

Add a command constant and payload type, then handle the command explicitly in `internal/transport/websocket/commands.go` and add protocol tests. Validate the connection role, authorization, payload, and any session ID before performing state-changing operations.
