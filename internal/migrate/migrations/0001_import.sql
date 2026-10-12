-- The importer's own state: what it has read, what it has written, and where it stopped.
--
-- Two decisions are worth reading before the columns.
--
-- 1. The ledger is append-only, not a table that is updated in place. A migration runs more than
--    once: an initial sweep, then a delta pass while the legacy backend is still live, then
--    another after a freeze. If the ledger held one row per document and overwrote it, it could
--    only answer "what is true now", which is the answer the target already gives. Appending rows
--    answers "what did we write, when, and from which checksum", which is what an operator needs
--    at 3am when a player says their gold is wrong. A trigger refuses updates and deletes, so the
--    property holds even if a later query tries.
--
-- 2. The checkpoint is one row per (source, stream) rather than one per batch. A run that dies
--    mid-batch has to resume at a batch boundary, and the newest batch number is the whole state
--    it needs to do that.

create table if not exists schema_version (
    version    integer primary key,
    applied_at timestamptz not null default now()
);

create table if not exists identity_map (
    source        text        not null,
    source_id     text        not null,
    custom_id     text        not null,
    nakama_user_id text       not null,
    first_seen_at timestamptz not null default now(),
    last_seen_at  timestamptz not null default now(),
    primary key (source, source_id),
    -- One custom id, one account. This is the constraint that makes "no duplicate account" a
    -- fact rather than a hope: a second insert with the same custom id fails here.
    unique (custom_id)
);

create table if not exists import_ledger (
    source      text        not null,
    entity      text        not null,
    source_id   text        not null,
    checksum    text        not null,
    run_id      bigint      not null,
    recorded_at timestamptz not null default now(),
    primary key (source, entity, source_id, checksum)
);

create index if not exists import_ledger_lookup on import_ledger (source, source_id, entity);

-- The newest checksum per document, which is the value a run compares against the source.
create or replace view import_ledger_latest as
select distinct on (source, entity, source_id)
       source, entity, source_id, checksum, run_id, recorded_at
  from import_ledger
 order by source, entity, source_id, recorded_at desc, run_id desc;

create or replace function import_ledger_is_append_only() returns trigger as $$
begin
    raise exception 'import_ledger is append-only: a migration records what it did, and history is not rewritten (attempted %)', tg_op;
end;
$$ language plpgsql;

drop trigger if exists import_ledger_no_update on import_ledger;
create trigger import_ledger_no_update
    before update or delete on import_ledger
    for each statement execute function import_ledger_is_append_only();

create table if not exists import_checkpoint (
    source       text        not null,
    stream       text        not null,
    cursor       text        not null,
    batch_no     integer     not null default 0,
    players_done bigint      not null default 0,
    updated_at   timestamptz not null default now(),
    primary key (source, stream)
);

create table if not exists import_run (
    id                 bigserial primary key,
    source             text        not null,
    strategy           text        not null,
    mode               text        not null,
    started_at         timestamptz not null default now(),
    finished_at        timestamptz,
    players_seen       bigint      not null default 0,
    players_missing    bigint      not null default 0,
    documents_written  bigint      not null default 0,
    documents_identical bigint     not null default 0,
    errors             bigint      not null default 0
);

create table if not exists import_mismatch (
    run_id     bigint not null,
    source_id  text   not null,
    entity     text   not null,
    detail     text   not null,
    primary key (run_id, source_id, entity)
);
