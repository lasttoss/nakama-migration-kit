# Provenance

Everything in this repository was written for it, on personal time, by Tran Trung Dung.

- A migration importer and a nakama runtime module, written to move players off a legacy backend. No employer code, no client code, no proprietary data. Nothing here is
  extracted from a product I worked on; it talks to the public HTTP and RPC API of a self-hosted Nakama, and PostgreSQL; the demo in this repository starts its own fake legacy service, its own PostgreSQL and its own Nakama container.
- Third-party code is not vendored: the dependencies are github.com/jackc/pgx (through the pgx/pgpassfile/pgservicefile/puddle modules) and, in the `runtime/` module, github.com/heroiclabs/nakama-common, and each keeps its own licence.
- The diagrams are drawn here. The commit history is the history of this repository.
- `scripts/chart-check.sh` renders the chart in CI and checks that the manifest still makes the claims the README does; it is part of the evidence, not decoration.
- The `runtime/` directory is a separate Go module because a Nakama plugin has to be built against the same runtime as the server that loads it; its provenance is next to it in `runtime/PROVENANCE.md`.

If you believe something here belongs to you, open an issue and it will be dealt with.
