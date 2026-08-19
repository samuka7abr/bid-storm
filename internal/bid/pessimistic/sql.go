package pessimistic

// lockAuction reads the row and holds it until the transaction ends.
//
// clock_timestamp(), not now(). Inside a transaction now() is
// transaction_timestamp(): the instant of the BEGIN, which happens before the
// wait on the lock. Under a thousand VUs a transaction can sit in the queue for
// hundreds of milliseconds and still read a now() from before that wait, which
// would leave the pessimistic closing guard looser than the optimistic one — and
// the difference would show up right at the ends_at boundary, the entire
// scenario of this project. It would look like a difference between strategies
// and be a difference between clocks (decisão 27).
//
// min_increment_cents comes out here because the 201 publishes minNextBid, and
// under the lock the row cannot change before the write: the value read is the
// value that decides, so the accept path never needs a second query.
const lockAuction = `
SELECT version, highest_bid_cents, min_increment_cents, status, ends_at, clock_timestamp()
  FROM auctions
 WHERE id = $1
   FOR UPDATE`

// placeBid writes, and carries no guard at all.
//
// No version, no status, no ends_at in the WHERE: the lock is the guard, and the
// classification in Go already decided against the very row this statement
// updates. Repeating the guards would be belt and braces with a hidden cost —
// a zero-row branch nobody knows how to classify, because under the lock it is
// unreachable. An unreachable branch is not safety, it is code that was never
// tested waiting for its first occasion.
//
// UPDATE and INSERT share one CTE, the same shape the optimistic engine uses.
// That is not the mechanism of pessimism, so it does not get to cost a
// round-trip: what pessimism pays for is Begin, the locked read and Commit
// (decisão 25). ins runs even though the final SELECT never reads it — a
// data-modifying CTE always executes to completion.
const placeBid = `
WITH upd AS (
    UPDATE auctions
       SET highest_bid_cents = $1,
           highest_bidder    = $2,
           version           = version + 1
     WHERE id = $3
    RETURNING version
),
ins AS (
    INSERT INTO bids (id, auction_id, user_id, amount_cents, seq)
    SELECT $4, $3, $2, $1, upd.version FROM upd
)
SELECT version FROM upd`
