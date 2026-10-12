# Mapping profiles

A PlayFab title has no schema: the game team picked the user data keys, the statistic names, the
currency codes and the catalog ids, and the next title picked different ones. The part of a
migration that is the same everywhere is the transport — listing players, paging, backing off a
429 — and that lives in `internal/playfab`. The part that differs is here, as one file per title.

A profile says:

| Field | Question it answers |
|---|---|
| `source` | which legacy backend these ids come from (part of every derived custom id) |
| `collection` | which Nakama storage collection the game actually reads |
| `currency` | which legacy code is which balance in the wallet |
| `user_data` | which legacy key carries level, energy, and the regeneration anchor |
| `statistics` | which legacy stats the game still reads |
| `unmapped` | `note`: import the rest and report what has no rule. `fail`: refuse to start |

`unmapped` is `note` at the start of a cutover on purpose: the coverage report is the point of
this file, and a migration that refuses to run until every key is classified never runs. Move it
to `fail` once the report is empty.

## The first step on a live title is read-only

Point the discovery pass at the tenant and read the list of names it actually uses, before
writing anything:

```
make discover PROFILE=profiles/acme.json PLAYFAB_TITLE=… PLAYFAB_SECRET=…
```

It reads a sample of players and prints every currency code, user data key, statistic and catalog
id it saw, how often each appears, and which of them the profile has no rule for. The draft
profile it prints is the starting point for the file above. Discovery never writes: it is safe to
run against production during play hours.

`profiles/faketenant.json` is the profile for the fake tenant the demo and the tests use, and it
is a real profile in the same format a real title gets.
