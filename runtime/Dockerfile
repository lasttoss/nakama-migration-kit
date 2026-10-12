# The plugin has to be built by the toolchain that built the server, so this image is the server it
# will be loaded into - same version, same Go, same nakama-common. A mismatch here does not fail at
# build time; the module simply does not load and the RPCs are not registered.
FROM heroiclabs/nakama-pluginbuilder:3.41.0 AS build
WORKDIR /app
COPY runtime/go.mod runtime/go.sum ./
RUN go mod download
COPY runtime/ ./
RUN go build --trimpath --buildmode=plugin -o modules/backend.so ./cmd/server

FROM heroiclabs/nakama:3.41.0
COPY --from=build /app/modules/backend.so /nakama/data/modules/backend.so
