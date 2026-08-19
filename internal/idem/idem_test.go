package idem_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/redis/go-redis/v9"

	"github.com/samuka7abr/bid-storm/internal/idem"
	"github.com/samuka7abr/bid-storm/internal/testsupport"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	os.Exit(m.Run())
}

// The body of a 201, as the bid handler would write it, and the content type
// gin puts on it. The middleware never parses the body — what it owes is to give
// these exact bytes back, under the same header.
const contentTypeJSON = "application/json; charset=utf-8"

const acceptedBody = `{"currentVersion":1,"currentHighestBid":100,"minNextBid":200,"bidId":"11111111-1111-4111-8111-111111111111","seq":1}`

// Without a key nothing is touched: no Redis command, no metric, no buffer.
// That is what makes the "no idempotency" control cell a choice of the load
// generator rather than an environment variable (decisão 41).
func TestWithoutAKeyTheMiddlewareIsAPassThrough(t *testing.T) {
	rdb := testsupport.StartRedis(t)
	observed := newObserved()
	calls := 0
	r := router(idem.NewStore(rdb.Client), observed.metrics, accept(&calls))

	before := dbSize(t, rdb)
	res := post(r, "", `{"amountCents":100}`)
	if res.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusCreated)
	}
	if calls != 1 {
		t.Errorf("handler calls = %d, want 1", calls)
	}
	if got := res.Header().Get(idem.HeaderReplayed); got != "" {
		t.Errorf("%s = %q, want absent", idem.HeaderReplayed, got)
	}
	if after := dbSize(t, rdb); after != before {
		t.Errorf("redis grew from %d to %d keys: a bid without a key must not touch it", before, after)
	}
	observed.wantHits(t, 0, 0)
	observed.wantAttempts(t, 0, 0)
}

// The key space is the only defence left once the body fingerprint is gone
// (decisão 32), so a key that is not a UUID is refused with the same shape
// RequireUserID already uses — and the engine is never consulted.
func TestAMalformedKeyIsRefused(t *testing.T) {
	rdb := testsupport.StartRedis(t)
	observed := newObserved()
	calls := 0
	r := router(idem.NewStore(rdb.Client), observed.metrics, accept(&calls))

	before := dbSize(t, rdb)
	res := post(r, "not-a-uuid", `{"amountCents":100}`)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusBadRequest)
	}
	if got := decode(t, res)["error"]; got != idem.CodeInvalidKey {
		t.Errorf("error = %v, want %q", got, idem.CodeInvalidKey)
	}
	if calls != 0 {
		t.Errorf("handler calls = %d, want 0", calls)
	}
	if after := dbSize(t, rdb); after != before {
		t.Errorf("redis grew from %d to %d keys on a malformed key", before, after)
	}
}

func TestTheFirstAttemptReachesTheHandlerAndIsStored(t *testing.T) {
	rdb := testsupport.StartRedis(t)
	observed := newObserved()
	key := uuid.NewString()

	var seen string
	r := router(idem.NewStore(rdb.Client), observed.metrics, func(c *gin.Context) {
		// The key travels in the request context because the bid handler must
		// not gain a line for idempotency: this is how it reaches the engine.
		seen = idem.KeyFrom(c.Request.Context())
		c.Data(http.StatusCreated, contentTypeJSON, []byte(acceptedBody))
	})

	res := post(r, key, `{"amountCents":100}`)
	if res.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusCreated)
	}
	if seen != key {
		t.Errorf("key in the request context = %q, want %q", seen, key)
	}
	if got := res.Header().Get(idem.HeaderReplayed); got != "" {
		t.Errorf("%s = %q, want absent on a fresh accept", idem.HeaderReplayed, got)
	}

	entry := hash(t, rdb, key)
	if entry["done"] != acceptedBody {
		t.Errorf("done = %q, want the 201 body", entry["done"])
	}
	if _, marked := entry["busy"]; marked {
		t.Error("busy survived the accept")
	}
	observed.wantHits(t, 0, 0)
	observed.wantAttempts(t, 1, 1)
}

// The promise of provas.md §3, and the condition spec 03 needs in order to
// tighten I5: the same 201, byte for byte, declared in a header.
func TestAResendReplaysTheStored201Verbatim(t *testing.T) {
	rdb := testsupport.StartRedis(t)
	observed := newObserved()
	calls := 0
	key := uuid.NewString()
	r := router(idem.NewStore(rdb.Client), observed.metrics, accept(&calls))

	first := post(r, key, `{"amountCents":100}`)
	second := post(r, key, `{"amountCents":100}`)

	if second.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", second.Code, http.StatusCreated)
	}
	if second.Body.String() != first.Body.String() {
		t.Errorf("replayed body = %q, want %q", second.Body.String(), first.Body.String())
	}
	if got := second.Header().Get(idem.HeaderReplayed); got != "true" {
		t.Errorf("%s = %q, want true", idem.HeaderReplayed, got)
	}
	if got := second.Header().Get("Content-Type"); got != contentTypeJSON {
		t.Errorf("content type = %q, want %q", got, contentTypeJSON)
	}
	if calls != 1 {
		t.Errorf("handler calls = %d, want 1: the engine must not see the duplicate", calls)
	}
	observed.wantHits(t, 1, 0)
	// A replay is not an attempt: nothing reached the engine under it.
	observed.wantAttempts(t, 1, 1)
}

// The case provas.md calls the interesting one. The handler is held inside the
// middleware, which is the only way to have two requests under one key in
// flight at the same time.
func TestADuplicateInFlightIsRefusedWithoutWaiting(t *testing.T) {
	rdb := testsupport.StartRedis(t)
	observed := newObserved()
	key := uuid.NewString()

	entered := make(chan struct{})
	release := make(chan struct{})
	r := router(idem.NewStore(rdb.Client), observed.metrics, func(c *gin.Context) {
		close(entered)
		<-release
		c.Data(http.StatusCreated, contentTypeJSON, []byte(acceptedBody))
	})

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- post(r, key, `{"amountCents":100}`) }()
	<-entered

	// Answering at all before release is closed is the assertion: it did not
	// wait for the original, which is what separates 425 from the 409-and-wait
	// that provas.md had written down (decisão 33).
	second := post(r, key, `{"amountCents":100}`)
	if second.Code != http.StatusTooEarly {
		t.Fatalf("status = %d, want %d", second.Code, http.StatusTooEarly)
	}

	body := decode(t, second)
	if body["error"] != idem.CodeInFlight {
		t.Errorf("error = %v, want %q", body["error"], idem.CodeInFlight)
	}
	if body["retryable"] != true {
		t.Errorf("retryable = %v, want true", body["retryable"])
	}
	// No engine was consulted, so there is no auction state to publish.
	for _, field := range []string{"currentVersion", "currentHighestBid", "minNextBid"} {
		if _, present := body[field]; present {
			t.Errorf("the 425 carries %s, and nothing read it", field)
		}
	}

	close(release)
	if first := <-firstDone; first.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want %d", first.Code, http.StatusCreated)
	}
	observed.wantHits(t, 0, 1)
	observed.wantAttempts(t, 1, 1)
}

// Decisão 31 executing: same key, different body, and the bid passes. Under
// request semantics this second attempt would come back as key reuse and the
// bidder would be stuck against the middleware.
func TestAReAimedRetryUnderTheSameKeyIsNotADuplicate(t *testing.T) {
	rdb := testsupport.StartRedis(t)
	observed := newObserved()
	key := uuid.NewString()

	calls := 0
	r := router(idem.NewStore(rdb.Client), observed.metrics, func(c *gin.Context) {
		calls++
		if calls == 1 {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "too_low", "retryable": true, "minNextBid": 200})
			return
		}
		c.Data(http.StatusCreated, contentTypeJSON, []byte(acceptedBody))
	})

	if rejected := post(r, key, `{"amountCents":100}`); rejected.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", rejected.Code, http.StatusUnprocessableEntity)
	}
	// A rejection stores nothing — the retry must not get the 422 back.
	if entry := hash(t, rdb, key); entry["attempts"] != "1" {
		t.Errorf("attempts field = %q, want 1", entry["attempts"])
	} else if _, marked := entry["busy"]; marked {
		t.Error("busy survived the rejection: the re-aimed retry would get a 425")
	}

	retried := post(r, key, `{"amountCents":200}`)
	if retried.Code != http.StatusCreated {
		t.Fatalf("retry status = %d, want %d", retried.Code, http.StatusCreated)
	}
	if calls != 2 {
		t.Errorf("handler calls = %d, want 2", calls)
	}

	// One accept, two attempts under the key: the thread the k6 held alone
	// until now, measured by the server.
	observed.wantAttempts(t, 1, 2)
	observed.wantHits(t, 0, 0)
}

// Failing closed. Letting the bid through unguarded would be a weaker promise
// made in silence, and it would not even avoid the error: both duplicates would
// reach the engine and the partial unique index would refuse the second one
// with a 503 anyway, after the work was spent (decisão 36).
func TestRedisDownFailsClosedAndOnlyForKeyedBids(t *testing.T) {
	observed := newObserved()
	calls := 0
	r := router(idem.NewStore(unreachable(t)), observed.metrics, accept(&calls))

	keyed := post(r, uuid.NewString(), `{"amountCents":100}`)
	if keyed.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", keyed.Code, http.StatusServiceUnavailable)
	}
	body := decode(t, keyed)
	if body["error"] != "unavailable" {
		t.Errorf("error = %v, want %q", body["error"], "unavailable")
	}
	if body["retryable"] != true {
		t.Errorf("retryable = %v, want true", body["retryable"])
	}
	if calls != 0 {
		t.Errorf("handler calls = %d, want 0", calls)
	}

	// A bid without a key never touches Redis, so a Redis on the floor does not
	// stop it. This is C6, and it is what keeps the control cell alive.
	if anonymous := post(r, "", `{"amountCents":100}`); anonymous.Code != http.StatusCreated {
		t.Errorf("status without a key = %d, want %d", anonymous.Code, http.StatusCreated)
	}

	// Nothing was turned away, so nothing is counted. Infrastructure failure is
	// not a sample.
	observed.wantHits(t, 0, 0)
}

func router(store *idem.Store, m idem.Metrics, handler gin.HandlerFunc) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	r.POST("/auctions/:id/bids", idem.Middleware(store, m, slog.New(slog.NewTextHandler(io.Discard, nil))), handler)
	return r
}

// accept is the bid handler as far as the middleware can see it: something that
// writes a 201 and a body.
func accept(calls *int) gin.HandlerFunc {
	return func(c *gin.Context) {
		*calls++
		c.Data(http.StatusCreated, contentTypeJSON, []byte(acceptedBody))
	}
}

func post(r http.Handler, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost,
		"/auctions/6f1b0d3e-0f6c-4a4a-9a7a-0f6c4a4a9a7a/bids", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-Id", "00000000-0000-4000-8000-000000000001")
	if key != "" {
		req.Header.Set(idem.HeaderKey, key)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, res *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	body := map[string]any{}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", res.Body.String(), err)
	}
	return body
}

// observed is the Metrics struct with the three primitives kept at hand. Plain
// Prometheus types rather than internal/metrics: what the middleware owes is to
// feed the observers it was handed, and the series names are that package's
// business (RF03).
type observed struct {
	metrics  idem.Metrics
	replayed prometheus.Counter
	inFlight prometheus.Counter
	attempts prometheus.Histogram
}

func newObserved() observed {
	replayed := prometheus.NewCounter(prometheus.CounterOpts{Name: "replayed", Help: "replays"})
	inFlight := prometheus.NewCounter(prometheus.CounterOpts{Name: "in_flight", Help: "in flight"})
	attempts := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "attempts", Help: "attempts", Buckets: prometheus.LinearBuckets(1, 1, 10),
	})
	return observed{
		metrics:  idem.Metrics{Replayed: replayed, InFlight: inFlight, Attempts: attempts},
		replayed: replayed,
		inFlight: inFlight,
		attempts: attempts,
	}
}

func (o observed) wantHits(t *testing.T, replayed, inFlight float64) {
	t.Helper()
	if got := testutil.ToFloat64(o.replayed); got != replayed {
		t.Errorf("replayed hits = %v, want %v", got, replayed)
	}
	if got := testutil.ToFloat64(o.inFlight); got != inFlight {
		t.Errorf("in_flight hits = %v, want %v", got, inFlight)
	}
}

func (o observed) wantAttempts(t *testing.T, count uint64, sum float64) {
	t.Helper()
	var m dto.Metric
	if err := o.attempts.Write(&m); err != nil {
		t.Fatalf("read the histogram: %v", err)
	}
	if got := m.GetHistogram().GetSampleCount(); got != count {
		t.Errorf("attempts samples = %d, want %d", got, count)
	}
	if got := m.GetHistogram().GetSampleSum(); got != sum {
		t.Errorf("attempts sum = %v, want %v", got, sum)
	}
}

func dbSize(t *testing.T, rdb *testsupport.Redis) int64 {
	t.Helper()
	size, err := rdb.Client.DBSize(context.Background()).Result()
	if err != nil {
		t.Fatalf("DBSIZE: %v", err)
	}
	return size
}

// unreachable is a client pointed at a port nobody listens on: the port is
// taken and released, so it is free and stays free for the length of the test.
func unreachable(t *testing.T) *redis.Client {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release the port: %v", err)
	}
	return redis.NewClient(&redis.Options{
		Addr:        addr,
		MaxRetries:  -1,
		DialTimeout: 200 * time.Millisecond,
	})
}
