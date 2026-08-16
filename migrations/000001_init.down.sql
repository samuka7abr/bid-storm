DROP INDEX IF EXISTS bids_auction_seq_idx;
DROP INDEX IF EXISTS bids_idempotency_key_uq;
DROP TABLE IF EXISTS bids;
DROP TABLE IF EXISTS auctions;
DROP TYPE IF EXISTS auction_status;
