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

> Phase 1 only documents the command — do **not** run code generation yet.

Prerequisites (install once, outside this phase):

```sh
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
```

Generate Go stubs from the project root:

```sh
protoc \
  --proto_path=proto \
  --go_out=. --go_opt=module=voicx \
  --go-grpc_out=. --go-grpc_opt=module=voicx \
  proto/signaling.proto \
  proto/chat.proto \
  proto/events.proto \
  proto/control.proto
```

This emits Go files under `voicx/v1/` matching the `go_package` option
(`voicx/v1;voicxv1`) declared in each `.proto` file.

Alternatively, with buf:

```sh
buf generate
```

(a `buf.gen.yaml` will be added in a later phase to configure this).
