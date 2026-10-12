# Provenance: `github.com/heroiclabs/nakama-common`

This module is a Go plugin loaded into Nakama's own process. A Go plugin is not a package: it is
linked into the host binary's runtime, and the host and the plugin must be built by the same
toolchain with the same `nakama-common` sources, or the load fails with a version mismatch rather
than with a compile error. So the versions here are not preferences, they are properties of the
server this kit runs against.

| | version | where it comes from |
|---|---|---|
| Nakama | `3.41.0` | the image the kit's lab and chart run |
| Go | `1.27.1` | read out of that server binary |
| `nakama-common` | `v1.48.0` | linked into that server binary |
| `protobuf` | `v1.36.12` | the version `nakama-common v1.48.0` requires |

Read from the server rather than from a release note, because a release note is about the repository
and this is about the binary:

```
docker create --name nk-tmp heroiclabs/nakama:3.41.0
docker cp nk-tmp:/nakama /tmp/nakama-bin
docker rm nk-tmp
go version -m /tmp/nakama-bin/nakama | grep -E "go1|nakama-common"
```

Two consequences, both of which cost an afternoon when ignored:

* **Build with `heroiclabs/nakama-pluginbuilder:3.41.0`, not with the Go installed here.** It is the
  image that carries go1.27.1. Its entrypoint is `go`, so a shell command needs
  `docker run --entrypoint sh`. `scripts/runtime-smoke.sh` and the CI job both do this; the local Go
  toolchain is used for the main module's tests, where it does not matter.
* **`go build ./...` does not build this module.** `cmd/server` is a plugin: it has an
  `InitModule` and no `main`, so the command is
  `go build --trimpath --buildmode=plugin -o modules/backend.so ./cmd/server`. `runtime/` is a
  separate Go module for the same reason - the main module must not make the plugin's pinned
  dependency part of its own build graph.

Upgrading Nakama means re-reading all four numbers and changing all four together. The smoke script
is what fails loudly when only two of them were changed: the module registers its RPCs on load, and
the script refuses to continue if it did not.
