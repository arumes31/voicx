# voicx Protocol Buffers

This directory defines the gRPC/Protobuf schema for the voicx voice/video
server. All files use `syntax = "proto3";` and the package `voicx.v1`.

## Files

| File | Service(s) | Purpose |
|------|------------|---------|
| [`signaling.proto`](signaling.proto) | `Signaling` | WebRTC signaling: SDP offer/answer, ICE candidates, channel join/leave, subscribe/unsubscribe. |
| [`chat.proto`](chat.proto) | `Chat` | Channel chat, global server chat, direct messages, offline message spool. |
| [`events.proto`](events.proto) | `Events` | Server events broadcast to clients (user joined/left, speaking, channel created/deleted, user moved/kicked/banned). |
| [`control.proto`](control.proto) | `Control` | Authentication, channel create/delete/list, permission queries, file transfer control. |

## Linting

A top-level [`buf.yaml`](../buf.yaml) configures the [buf](https://buf.build)
CLI for linting and breaking-change detection. To lint the schema:

```sh
buf lint
```

## Generating Go code

The Go stubs are generated and committed under [`v1/`](../v1) (package
`voicxv1`), matching the `go_package` option (`voicx/v1;voicxv1`) declared in
each `.proto` file. Regenerate them after every schema change and commit the
result — the server (232) compiles against them.

Prerequisites (install once):

```sh
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
go install github.com/bufbuild/buf/cmd/buf@latest
```

Regenerate from the project root ([`buf.gen.yaml`](../buf.gen.yaml) configures
the plugins and the output layout):

```sh
buf generate
```

buf compiles the schema itself, so `protoc` is not needed.

## Implementation status

| Service | Status |
|---------|--------|
| `Events` | Served: `Subscribe` streams from the server-side event bus. |
| `Control` | Served: auth, channel create/delete/list, permission query. The file-transfer RPCs return `Unimplemented` on purpose — transfer tokens are minted by the control channel after a per-client permission check. |
| `Chat` | Not served: chat is end-to-end/scope-key encrypted on the control channel. |
| `Signaling` | Not served: WebRTC signaling stays on the control channel. |
