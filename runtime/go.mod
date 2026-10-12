// The runtime module is a Go plugin Nakama loads, so it is built with the toolchain that built the
// server it will be loaded into, not with whatever Go is installed here: see PROVENANCE.md.
module github.com/lasttoss/nakama-migration-kit/runtime

go 1.27.1

require github.com/heroiclabs/nakama-common v1.48.0

require google.golang.org/protobuf v1.36.12 // indirect
