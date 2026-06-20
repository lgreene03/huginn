# 0006. Dynamic-proto gRPC service without protoc or generated stubs

- **Status:** Accepted
- **Date:** 2026-06-20
- **Deciders:** Project maintainer
- **Related:** [ADR-0005 — Multi-topic fan-in](0005-single-goroutine-dispatch-multi-topic-fanin.md), [architecture.md](../architecture.md)

## Context

Huginn exposes its portfolio/strategy snapshot over HTTP (`/api/snapshot`, SSE) for the operator dashboard. A programmatic client (a research script, a stack-level health probe) wants the same snapshot over gRPC: a single unary `GetSnapshot` returning the snapshot JSON, at lower overhead than the SSE stream, with server reflection so `grpcurl` works without a local `.proto`.

The conventional path is to author a `service.proto`, run `protoc` with the Go plugins, and check in (or generate-in-CI) the stubs. For exactly one method whose response is a JSON string and an HTTP-style status code, that toolchain is heavy: it adds `protoc` + plugin version pins to every build and CI environment, a generation step, and a tree of generated files — all to describe a two-field message that will essentially never change.

## Decision

Huginn builds the gRPC service **descriptor at runtime** from `descriptorpb` and serves it with `dynamicpb`, registered exactly once via a `sync.Once`. No `.proto` file, no `protoc`, no generated Go. The `SnapshotResponse` message (`json_payload string`, `status_code int32`) and the `HuginnService/GetSnapshot` method are constructed as a `FileDescriptorProto`, turned into a live `protoreflect.FileDescriptor`, and registered in the global proto registry. The handler fills a `dynamicpb.NewMessage` with the marshaled snapshot. `reflection.Register` is enabled so clients introspect the schema over the wire.

The justification is proportionality: the surface is one method whose payload is opaque JSON. The dynamic descriptor *is* the schema definition, lives next to the handler in one file, and removes a build-time dependency that would otherwise have to exist in every developer machine and CI image for no recurring benefit. The gRPC server is opt-in (`GRPC_PORT` unset = disabled), so it costs nothing when unused.

## Consequences

**Easier.**

- No `protoc`/plugin toolchain in the build or CI — `go build ./...` is the whole story.
- The schema and the handler are in one file (`grpc.go`); there is no generated code to regenerate or drift.
- Server reflection still works, so `grpcurl localhost:50051 huginn.HuginnService/GetSnapshot` needs no local proto.

**Harder / cost.**

- No generated client stubs and no compile-time type safety on the message. Clients either use reflection or hand-build the request; a Go consumer doesn't get a typed `SnapshotResponse`. Acceptable because the payload is already opaque JSON the client must parse anyway.
- The descriptor is registered in the *global* proto registry. Two registrations of the same file would conflict; `sync.Once` plus warn-on-already-registered guards this within a process, but it means the service name/file path are effectively process-global identifiers.
- If the service ever grows to several methods or strongly-typed messages, the hand-built descriptor stops being proportional and this decision should be revisited (and superseded) in favour of a real `.proto` + generated stubs.

## Alternatives Considered

- **`protoc` + generated Go stubs.** Rejected *for the current surface*. Correct and conventional, but adds a toolchain dependency to every build/CI environment to describe one trivial, rarely-changing message. The right choice once the API is non-trivial.
- **HTTP/JSON only, no gRPC.** Considered. The dashboard already uses HTTP. gRPC was wanted specifically for a typed-transport, reflection-introspectable programmatic entry point at lower overhead than SSE; dropping it would remove that.
- **A third-party "proto-from-struct" codegen library.** Rejected. Adds a dependency to avoid writing ~40 lines of descriptor that are themselves the clearest statement of the schema.

## References

- [`internal/server/grpc.go`](../../internal/server/grpc.go) — `ensureProtoRegistered` (runtime `FileDescriptorProto`), the hand-built `grpc.ServiceDesc`, `dynamicpb`-backed handler, and reflection registration.
- [protobuf-go `dynamicpb`](https://pkg.go.dev/google.golang.org/protobuf/types/dynamicpb) and [`protodesc`](https://pkg.go.dev/google.golang.org/protobuf/reflect/protodesc) — the libraries this leans on.
