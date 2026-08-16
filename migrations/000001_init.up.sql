CREATE TYPE auction_status AS ENUM ('open', 'closed');

CREATE TABLE auctions (
    id                  uuid PRIMARY KEY,
    title               text NOT NULL,
    status              auction_status NOT NULL DEFAULT 'open',
    highest_bid_cents   bigint NOT NULL DEFAULT 0 CHECK (highest_bid_cents >= 0),
    highest_bidder      uuid,
    min_increment_cents bigint NOT NULL DEFAULT 100 CHECK (min_increment_cents > 0),
    version             bigint NOT NULL DEFAULT 0,
    ends_at             timestamptz NOT NULL,
    closed_at           timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE bids (
    id              uuid PRIMARY KEY,
    auction_id      uuid NOT NULL REFERENCES auctions(id) ON DELETE CASCADE,
    user_id         uuid NOT NULL,
    amount_cents    bigint NOT NULL CHECK (amount_cents > 0),
    seq             bigint NOT NULL,
    idempotency_key text,
    created_at      timestamptz NOT NULL DEFAULT now(),

    UNIQUE (auction_id, seq)
);

CREATE UNIQUE INDEX bids_idempotency_key_uq
    ON bids (idempotency_key) WHERE idempotency_key IS NOT NULL;

CREATE INDEX bids_auction_seq_idx ON bids (auction_id, seq DESC);
