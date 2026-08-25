# voicx Protocol Buffers

This directory defines the gRPC/Protobuf schema for the voicx voice/video
server. All files use `syntax = "proto3";` and the package `voicx.v1`.

## Files

| File | Service(s) | Purpose |
|------|------------|---------|
| [`signaling.proto`](signaling.proto) | `Signaling` (deprecated) | Compatibility descriptors for the intentionally unserved WebRTC signaling RPCs. |
| [`chat.proto`](chat.proto) | `Chat` (deprecated) | Compatibility descriptors for the intentionally unserved chat RPCs. |
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

Prerequisite (install once):

```sh
go install github.com/bufbuild/buf/cmd/buf@v1.72.0
```

Regenerate from the project root ([`buf.gen.yaml`](../buf.gen.yaml) configures
the pinned remote plugins and output layout):

```sh
buf generate
```

buf compiles the schema itself and downloads the pinned remote Go plugins, so
neither `protoc` nor local `protoc-gen-*` binaries are needed.

## Implementation status

| Service | Status |
|---------|--------|
| `Events` | Served: `Subscribe` streams from the server-side event bus. |
| `Control` | Served: auth, channel create/delete/list, permission query. Authentication returns only `user_id`; it does not mint a session token. The file-transfer RPCs intentionally return `Unimplemented` — transfer tokens are minted by the control channel after a per-client permission check. |
| `Chat` | Deprecated and intentionally unserved: chat is end-to-end/scope-key encrypted on the control channel. |
| `Signaling` | Deprecated and intentionally unserved: WebRTC signaling stays on the control channel. |
