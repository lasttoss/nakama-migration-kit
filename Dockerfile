# The kit's own binaries. Three commands in one image because they are one kit: an importer, a
# reconciler, and the fake title the lab starts.
FROM golang:1.27.1 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY internal/ internal/
COPY cmd/ cmd/
RUN CGO_ENABLED=0 go build -trimpath -o /out/importer ./cmd/importer \
 && CGO_ENABLED=0 go build -trimpath -o /out/reconcile ./cmd/reconcile \
 && CGO_ENABLED=0 go build -trimpath -o /out/faketenant ./cmd/faketenant

# distroless: no shell, no package manager. An importer runs unattended against a live title, and an
# image that cannot be exec'd into is one fewer way to change what a run did.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/importer /importer
COPY --from=build /out/reconcile /reconcile
COPY --from=build /out/faketenant /faketenant
COPY profiles/ /profiles/
USER nonroot
ENTRYPOINT ["/importer"]
