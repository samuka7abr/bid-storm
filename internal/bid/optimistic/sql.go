package optimistic

// placeBid is the happy path, and it is one statement.
//
// estrategias.md illustrates the mechanism as Begin → UPDATE ... RETURNING →
// INSERT → Commit. That is four round-trips per attempt with the connection held
// across all four. A data-modifying CTE is one round-trip and is just as atomic:
// if the UPDATE matches nothing, upd is empty, ins inserts nothing, and the
// final SELECT returns zero rows — pgx.ErrNoRows, which is what triggers the
// classification.
//
// The pessimistic engine cannot do this, because SELECT ... FOR UPDATE needs an
// open transaction. That asymmetry is deliberate and recorded in decisão 19:
// needing a transaction is a real cost of pessimism, and handing the optimistic
// engine a free transaction to level the two would hide that cost and tilt the
// benchmark towards the project's own hypothesis.
//
// min_increment_cents comes out of the final SELECT rather than the INSERT's
// RETURNING, which only sees columns of the table it wrote. It is here because
// the 201 publishes minNextBid, and without it the happy path would need a
// second query just to fill in the body.
const placeBid = `
WITH upd AS (
    UPDATE auctions
       SET highest_bid_cents = $1,
           highest_bidder    = $2,
           version           = version + 1
     WHERE id      = $3
       AND version = $4
       AND status  = 'open'
       AND now() < ends_at
       AND highest_bid_cents + min_increment_cents <= $1
    RETURNING version, min_increment_cents
),
ins AS (
    INSERT INTO bids (id, auction_id, user_id, amount_cents, seq)
    SELECT $5, $3, $2, $1, upd.version FROM upd
    RETURNING seq
)
SELECT ins.seq, upd.min_increment_cents FROM ins, upd`

// classify reads a fresh snapshot, and pays for it only on the failing path.
//
// Reading the state inside the same CTE was the obvious alternative and is
// wrong: every CTE of one statement shares the snapshot taken at its start, so
// the state reported in a 409 would predate the commit of the winner that just
// got ahead. The bidder re-aims at minNextBid, and re-aiming at a stale number
// produces a retry that is doomed before it leaves — bid_attempts_per_accept
// would climb as an artefact of the implementation rather than from contention.
//
// now() travels with the columns because the closing rule has to be decided by
// the same clock that decided the accept. It costs no extra round-trip.
const classify = `
SELECT version, highest_bid_cents, min_increment_cents, status, ends_at, now()
  FROM auctions
 WHERE id = $1`
