-- The kit's ledger and checkpoints live beside Nakama's own database rather than inside it: they are
-- this kit's bookkeeping, and a Nakama upgrade should not have to consider them.
CREATE DATABASE mkit;
