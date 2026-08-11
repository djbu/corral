-- M10a remembers accepted inbound provider messages by a one-way identifier
-- hash. The raw message id, sender, reply text, and provider token never enter
-- SQLite. Keeping this ledger durable makes a provider retry after a daemon
-- restart idempotent before any bytes can be written to a PTY.
CREATE TABLE incoming_messages (
    backend TEXT NOT NULL,
    message_id_hash TEXT NOT NULL,
    received_at_ms INTEGER NOT NULL,
    expires_at_ms INTEGER NOT NULL,
    PRIMARY KEY (backend, message_id_hash)
);

CREATE INDEX incoming_messages_expires_at_idx
    ON incoming_messages(expires_at_ms);
