// Package httpapi exposes the ledger over JSON/HTTP.
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/UncleKweks/ledger-service/internal/ledger"
)

const maxBodyBytes = 1 << 20

// NewHandler returns the HTTP handler for the ledger API.
func NewHandler(svc *ledger.Service, log *slog.Logger) http.Handler {
	h := &handler{svc: svc, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", h.health)
	mux.HandleFunc("POST /accounts", h.createAccount)
	mux.HandleFunc("GET /accounts/{id}", h.getAccount)
	mux.HandleFunc("GET /accounts/{id}/balance", h.getBalance)
	mux.HandleFunc("GET /accounts/{id}/entries", h.listEntries)
	mux.HandleFunc("POST /transactions", h.postTransaction)
	mux.HandleFunc("GET /transactions/{id}", h.getTransaction)
	return logRequests(log, mux)
}

type handler struct {
	svc *ledger.Service
	log *slog.Logger
}

// Wire types. Amounts are integers in minor units.

type accountJSON struct {
	ID                   uuid.UUID `json:"id"`
	Name                 string    `json:"name"`
	Currency             string    `json:"currency"`
	AllowNegativeBalance bool      `json:"allow_negative_balance"`
	CreatedAt            time.Time `json:"created_at"`
}

type balanceJSON struct {
	AccountID uuid.UUID `json:"account_id"`
	Currency  string    `json:"currency"`
	Balance   int64     `json:"balance"`
}

type entryJSON struct {
	ID            uuid.UUID `json:"id"`
	TransactionID uuid.UUID `json:"transaction_id"`
	AccountID     uuid.UUID `json:"account_id"`
	Currency      string    `json:"currency"`
	Amount        int64     `json:"amount"`
	CreatedAt     time.Time `json:"created_at"`
}

type transactionJSON struct {
	ID             uuid.UUID   `json:"id"`
	IdempotencyKey string      `json:"idempotency_key"`
	Description    string      `json:"description"`
	Entries        []entryJSON `json:"entries"`
	CreatedAt      time.Time   `json:"created_at"`
}

type createAccountRequest struct {
	Name                 string `json:"name"`
	Currency             string `json:"currency"`
	AllowNegativeBalance bool   `json:"allow_negative_balance"`
}

type postTransactionRequest struct {
	Description string `json:"description"`
	Entries     []struct {
		AccountID uuid.UUID `json:"account_id"`
		Currency  string    `json:"currency"`
		Amount    int64     `json:"amount"`
	} `json:"entries"`
}

func toAccountJSON(a ledger.Account) accountJSON {
	return accountJSON{a.ID, a.Name, a.Currency, a.AllowNegativeBalance, a.CreatedAt}
}

func toEntryJSON(e ledger.Entry) entryJSON {
	return entryJSON{e.ID, e.TransactionID, e.AccountID, e.Currency, e.Amount, e.CreatedAt}
}

func toTransactionJSON(t ledger.Transaction) transactionJSON {
	entries := make([]entryJSON, len(t.Entries))
	for i, e := range t.Entries {
		entries[i] = toEntryJSON(e)
	}
	return transactionJSON{t.ID, t.IdempotencyKey, t.Description, entries, t.CreatedAt}
}

// Handlers.

func (h *handler) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *handler) createAccount(w http.ResponseWriter, r *http.Request) {
	var req createAccountRequest
	if !h.decode(w, r, &req) {
		return
	}
	acc, err := h.svc.CreateAccount(r.Context(), ledger.NewAccount{
		Name: req.Name, Currency: req.Currency, AllowNegativeBalance: req.AllowNegativeBalance,
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	w.Header().Set("Location", "/accounts/"+acc.ID.String())
	writeJSON(w, http.StatusCreated, toAccountJSON(acc))
}

func (h *handler) getAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r)
	if !ok {
		return
	}
	acc, err := h.svc.GetAccount(r.Context(), id)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toAccountJSON(acc))
}

func (h *handler) getBalance(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r)
	if !ok {
		return
	}
	b, err := h.svc.GetBalance(r.Context(), id)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, balanceJSON{b.AccountID, b.Currency, b.Amount})
}

func (h *handler) listEntries(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r)
	if !ok {
		return
	}
	limit := 0
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			writeProblem(w, http.StatusBadRequest, "invalid_request", "limit must be a positive integer")
			return
		}
		limit = n
	}
	entries, err := h.svc.ListEntries(r.Context(), id, limit)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	out := make([]entryJSON, len(entries))
	for i, e := range entries {
		out[i] = toEntryJSON(e)
	}
	writeJSON(w, http.StatusOK, map[string][]entryJSON{"entries": out})
}

func (h *handler) postTransaction(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeProblem(w, http.StatusBadRequest, "missing_idempotency_key", "the Idempotency-Key header is required")
		return
	}
	var req postTransactionRequest
	if !h.decode(w, r, &req) {
		return
	}
	nt := ledger.NewTransaction{IdempotencyKey: key, Description: req.Description}
	for _, e := range req.Entries {
		nt.Entries = append(nt.Entries, ledger.NewEntry{AccountID: e.AccountID, Currency: e.Currency, Amount: e.Amount})
	}

	txn, created, err := h.svc.PostTransaction(r.Context(), nt)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	w.Header().Set("Location", "/transactions/"+txn.ID.String())
	if !created {
		w.Header().Set("Idempotent-Replayed", "true")
		writeJSON(w, http.StatusOK, toTransactionJSON(txn))
		return
	}
	writeJSON(w, http.StatusCreated, toTransactionJSON(txn))
}

func (h *handler) getTransaction(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r)
	if !ok {
		return
	}
	txn, err := h.svc.GetTransaction(r.Context(), id)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toTransactionJSON(txn))
}

// Helpers.

func (h *handler) pathID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_request", "id must be a UUID")
		return uuid.Nil, false
	}
	return id, true
}

// decode reads a JSON body into v, rejecting unknown fields and bodies over
// maxBodyBytes. It writes a 400 and returns false on failure.
func (h *handler) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_json", fmt.Sprintf("invalid request body: %v", err))
		return false
	}
	return true
}

// writeError maps ledger errors to HTTP statuses. Unexpected errors are logged
// and reported as a generic 500 so internals never leak to clients.
func (h *handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ledger.ErrUnbalanced):
		writeProblem(w, http.StatusUnprocessableEntity, "unbalanced", err.Error())
	case errors.Is(err, ledger.ErrInvalid):
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_request", err.Error())
	case errors.Is(err, ledger.ErrInsufficientFunds):
		writeProblem(w, http.StatusUnprocessableEntity, "insufficient_funds", err.Error())
	case errors.Is(err, ledger.ErrCurrencyMismatch):
		writeProblem(w, http.StatusUnprocessableEntity, "currency_mismatch", err.Error())
	case errors.Is(err, ledger.ErrAccountNotFound):
		writeProblem(w, http.StatusNotFound, "account_not_found", err.Error())
	case errors.Is(err, ledger.ErrTransactionNotFound):
		writeProblem(w, http.StatusNotFound, "transaction_not_found", err.Error())
	case errors.Is(err, ledger.ErrIdempotencyConflict):
		writeProblem(w, http.StatusConflict, "idempotency_conflict", err.Error())
	default:
		h.log.ErrorContext(r.Context(), "request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		writeProblem(w, http.StatusInternalServerError, "internal", "internal server error")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeProblem(w http.ResponseWriter, status int, code, message string) {
	type problem struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	writeJSON(w, status, map[string]problem{"error": {code, message}})
}

// logRequests logs one line per request with its status and duration.
func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.InfoContext(r.Context(), "http request",
			"method", r.Method, "path", r.URL.Path, "status", rec.status, "duration_ms", time.Since(start).Milliseconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}
