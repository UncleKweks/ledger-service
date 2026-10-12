package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/UncleKweks/ledger-service/internal/httpapi"
	"github.com/UncleKweks/ledger-service/internal/ledger"
	"github.com/UncleKweks/ledger-service/internal/store/postgres"
	"github.com/UncleKweks/ledger-service/internal/testdb"
)

type client struct {
	t   *testing.T
	srv *httptest.Server
}

func newClient(t *testing.T) *client {
	t.Helper()
	svc := ledger.NewService(postgres.New(testdb.New(t)))
	srv := httptest.NewServer(httpapi.NewHandler(svc, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(srv.Close)
	return &client{t: t, srv: srv}
}

// do sends a request and decodes the JSON response into out (if non-nil).
func (c *client) do(method, path string, headers map[string]string, body any, out any) *http.Response {
	c.t.Helper()
	var r io.Reader
	if s, ok := body.(string); ok {
		r = bytes.NewBufferString(s)
	} else if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.srv.URL+path, r)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.srv.Client().Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			c.t.Fatalf("%s %s: decode: %v", method, path, err)
		}
	}
	return resp
}

type account struct {
	ID string `json:"id"`
}

type txnResp struct {
	ID      string `json:"id"`
	Entries []struct {
		Amount int64 `json:"amount"`
	} `json:"entries"`
}

type errResp struct {
	Error struct {
		Code string `json:"code"`
	} `json:"error"`
}

func (c *client) createAccount(name string, allowNegative bool) string {
	c.t.Helper()
	var a account
	resp := c.do("POST", "/accounts", nil,
		map[string]any{"name": name, "currency": "NGN", "allow_negative_balance": allowNegative}, &a)
	if resp.StatusCode != http.StatusCreated {
		c.t.Fatalf("create account: status %d", resp.StatusCode)
	}
	return a.ID
}

func transferBody(from, to string, amount int64) map[string]any {
	return map[string]any{
		"description": "transfer",
		"entries": []map[string]any{
			{"account_id": from, "currency": "NGN", "amount": -amount},
			{"account_id": to, "currency": "NGN", "amount": amount},
		},
	}
}

func TestTransferAndIdempotentReplay(t *testing.T) {
	c := newClient(t)
	world := c.createAccount("world", true)
	alice := c.createAccount("alice", false)
	key := map[string]string{"Idempotency-Key": "http-fund-1"}

	var first txnResp
	resp := c.do("POST", "/transactions", key, transferBody(world, alice, 5_000), &first)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first post: status %d, want 201", resp.StatusCode)
	}
	if resp.Header.Get("Idempotent-Replayed") != "" {
		t.Error("first post marked as replayed")
	}

	var replay txnResp
	resp = c.do("POST", "/transactions", key, transferBody(world, alice, 5_000), &replay)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: status %d, Idempotent-Replayed=%q", resp.StatusCode, resp.Header.Get("Idempotent-Replayed"))
	}
	if replay.ID != first.ID {
		t.Errorf("replay id = %s, want %s", replay.ID, first.ID)
	}

	var bal struct {
		Balance int64 `json:"balance"`
	}
	c.do("GET", "/accounts/"+alice+"/balance", nil, nil, &bal)
	if bal.Balance != 5_000 {
		t.Errorf("balance = %d, want 5000", bal.Balance)
	}

	var got txnResp
	if resp := c.do("GET", "/transactions/"+first.ID, nil, nil, &got); resp.StatusCode != http.StatusOK || len(got.Entries) != 2 {
		t.Errorf("get transaction: status %d, %d entries", resp.StatusCode, len(got.Entries))
	}

	var list struct {
		Entries []json.RawMessage `json:"entries"`
	}
	c.do("GET", "/accounts/"+alice+"/entries?limit=10", nil, nil, &list)
	if len(list.Entries) != 1 {
		t.Errorf("alice has %d entries, want 1", len(list.Entries))
	}
}

func TestErrorResponses(t *testing.T) {
	c := newClient(t)
	world := c.createAccount("world", true)
	alice := c.createAccount("alice", false)
	c.do("POST", "/transactions", map[string]string{"Idempotency-Key": "fund"}, transferBody(world, alice, 100), nil)

	key := func(k string) map[string]string { return map[string]string{"Idempotency-Key": k} }
	tests := []struct {
		name       string
		method     string
		path       string
		headers    map[string]string
		body       any
		wantStatus int
		wantCode   string
	}{
		{"missing idempotency key", "POST", "/transactions", nil, transferBody(world, alice, 1), 400, "missing_idempotency_key"},
		{"malformed json", "POST", "/transactions", key("k1"), "{", 400, "invalid_json"},
		{"unknown field", "POST", "/accounts", nil, `{"name":"x","currency":"NGN","curency":"USD"}`, 400, "invalid_json"},
		{"unbalanced", "POST", "/transactions", key("k2"), map[string]any{"entries": []map[string]any{
			{"account_id": world, "currency": "NGN", "amount": -10},
			{"account_id": alice, "currency": "NGN", "amount": 9},
		}}, 422, "unbalanced"},
		{"insufficient funds", "POST", "/transactions", key("k3"), transferBody(alice, world, 101), 422, "insufficient_funds"},
		{"bad currency", "POST", "/accounts", nil, map[string]any{"name": "x", "currency": "NAIRA"}, 422, "invalid_request"},
		{"idempotency conflict", "POST", "/transactions", key("fund"), transferBody(world, alice, 999), 409, "idempotency_conflict"},
		{"account not found", "GET", "/accounts/00000000-0000-0000-0000-000000000001", nil, nil, 404, "account_not_found"},
		{"transaction not found", "GET", "/transactions/00000000-0000-0000-0000-000000000001", nil, nil, 404, "transaction_not_found"},
		{"invalid id", "GET", "/accounts/not-a-uuid/balance", nil, nil, 400, "invalid_request"},
		{"invalid limit", "GET", "/accounts/" + alice + "/entries?limit=-1", nil, nil, 400, "invalid_request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var e errResp
			resp := c.do(tt.method, tt.path, tt.headers, tt.body, &e)
			if resp.StatusCode != tt.wantStatus || e.Error.Code != tt.wantCode {
				t.Errorf("got %d %q, want %d %q", resp.StatusCode, e.Error.Code, tt.wantStatus, tt.wantCode)
			}
		})
	}
}
