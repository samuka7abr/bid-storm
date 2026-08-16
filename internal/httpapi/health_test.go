package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/samuka7abr/bid-storm/internal/httpapi"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	m.Run()
}

func ok(context.Context) error { return nil }

func failing(msg string) httpapi.Probe {
	return func(context.Context) error { return errors.New(msg) }
}

func get(t *testing.T, r http.Handler, path string) (int, map[string]string) {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

	body := map[string]string{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s body %q: %v", path, rec.Body.String(), err)
	}
	return rec.Code, body
}

// Liveness must not depend on the database, or a schema divergence gets read as
// a dead process and restarted forever.
func TestHealthzIsGreenWithoutDatabase(t *testing.T) {
	r := httpapi.New(httpapi.Deps{
		Ping:    failing("connection refused"),
		Schema:  failing("schema version mismatch"),
		Metrics: http.NotFoundHandler(),
	})

	code, body := get(t, r, "/healthz")
	if code != http.StatusOK {
		t.Errorf("status = %d, want %d", code, http.StatusOK)
	}
	if body["status"] != "ok" {
		t.Errorf("status field = %q, want %q", body["status"], "ok")
	}
}

func TestReadyz(t *testing.T) {
	tests := []struct {
		name      string
		ping      httpapi.Probe
		schema    httpapi.Probe
		wantCode  int
		wantCheck string
	}{
		{"no database", failing("connection refused"), ok, http.StatusServiceUnavailable, "database"},
		{"wrong schema", ok, failing("schema version mismatch: expected 1, found 99"), http.StatusServiceUnavailable, "schema"},
		{"both fine", ok, ok, http.StatusOK, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httpapi.New(httpapi.Deps{Ping: tc.ping, Schema: tc.schema, Metrics: http.NotFoundHandler()})

			code, body := get(t, r, "/readyz")
			if code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body %v)", code, tc.wantCode, body)
			}
			// The body has to name the failing condition: "cannot reach Postgres"
			// and "Postgres has the wrong schema" need opposite reactions.
			if body["check"] != tc.wantCheck {
				t.Errorf("check = %q, want %q", body["check"], tc.wantCheck)
			}
		})
	}
}
