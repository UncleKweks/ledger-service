// Package grpcapi exposes the ledger over gRPC.
package grpcapi

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	ledgerv1 "github.com/UncleKweks/ledger-service/gen/ledger/v1"
	"github.com/UncleKweks/ledger-service/internal/ledger"
)

// Server implements ledgerv1.LedgerServiceServer on top of ledger.Service.
type Server struct {
	ledgerv1.UnimplementedLedgerServiceServer
	svc *ledger.Service
	log *slog.Logger
}

// NewServer returns a gRPC ledger server.
func NewServer(svc *ledger.Service, log *slog.Logger) *Server {
	return &Server{svc: svc, log: log}
}

func (s *Server) CreateAccount(ctx context.Context, req *ledgerv1.CreateAccountRequest) (*ledgerv1.CreateAccountResponse, error) {
	acc, err := s.svc.CreateAccount(ctx, ledger.NewAccount{
		Name: req.GetName(), Currency: req.GetCurrency(), AllowNegativeBalance: req.GetAllowNegativeBalance(),
	})
	if err != nil {
		return nil, s.toStatus(ctx, err)
	}
	return &ledgerv1.CreateAccountResponse{Account: toAccountPB(acc)}, nil
}

func (s *Server) GetAccount(ctx context.Context, req *ledgerv1.GetAccountRequest) (*ledgerv1.GetAccountResponse, error) {
	id, err := parseID("id", req.GetId())
	if err != nil {
		return nil, err
	}
	acc, err := s.svc.GetAccount(ctx, id)
	if err != nil {
		return nil, s.toStatus(ctx, err)
	}
	return &ledgerv1.GetAccountResponse{Account: toAccountPB(acc)}, nil
}

func (s *Server) GetBalance(ctx context.Context, req *ledgerv1.GetBalanceRequest) (*ledgerv1.GetBalanceResponse, error) {
	id, err := parseID("account_id", req.GetAccountId())
	if err != nil {
		return nil, err
	}
	b, err := s.svc.GetBalance(ctx, id)
	if err != nil {
		return nil, s.toStatus(ctx, err)
	}
	return &ledgerv1.GetBalanceResponse{AccountId: b.AccountID.String(), Currency: b.Currency, Balance: b.Amount}, nil
}

func (s *Server) ListEntries(ctx context.Context, req *ledgerv1.ListEntriesRequest) (*ledgerv1.ListEntriesResponse, error) {
	id, err := parseID("account_id", req.GetAccountId())
	if err != nil {
		return nil, err
	}
	entries, err := s.svc.ListEntries(ctx, id, int(req.GetLimit()))
	if err != nil {
		return nil, s.toStatus(ctx, err)
	}
	out := make([]*ledgerv1.Entry, len(entries))
	for i, e := range entries {
		out[i] = toEntryPB(e)
	}
	return &ledgerv1.ListEntriesResponse{Entries: out}, nil
}

func (s *Server) PostTransaction(ctx context.Context, req *ledgerv1.PostTransactionRequest) (*ledgerv1.PostTransactionResponse, error) {
	nt := ledger.NewTransaction{IdempotencyKey: req.GetIdempotencyKey(), Description: req.GetDescription()}
	for _, e := range req.GetEntries() {
		id, err := parseID("entries.account_id", e.GetAccountId())
		if err != nil {
			return nil, err
		}
		nt.Entries = append(nt.Entries, ledger.NewEntry{AccountID: id, Currency: e.GetCurrency(), Amount: e.GetAmount()})
	}
	txn, created, err := s.svc.PostTransaction(ctx, nt)
	if err != nil {
		return nil, s.toStatus(ctx, err)
	}
	return &ledgerv1.PostTransactionResponse{Transaction: toTransactionPB(txn), Replayed: !created}, nil
}

func (s *Server) GetTransaction(ctx context.Context, req *ledgerv1.GetTransactionRequest) (*ledgerv1.GetTransactionResponse, error) {
	id, err := parseID("id", req.GetId())
	if err != nil {
		return nil, err
	}
	txn, err := s.svc.GetTransaction(ctx, id)
	if err != nil {
		return nil, s.toStatus(ctx, err)
	}
	return &ledgerv1.GetTransactionResponse{Transaction: toTransactionPB(txn)}, nil
}

func parseID(field, v string) (uuid.UUID, error) {
	id, err := uuid.Parse(v)
	if err != nil {
		return uuid.Nil, status.Errorf(codes.InvalidArgument, "%s must be a UUID", field)
	}
	return id, nil
}

// toStatus maps ledger errors to gRPC status codes. Unexpected errors are
// logged and reported as INTERNAL without details.
func (s *Server) toStatus(ctx context.Context, err error) error {
	var code codes.Code
	switch {
	case errors.Is(err, ledger.ErrInvalid):
		code = codes.InvalidArgument
	case errors.Is(err, ledger.ErrAccountNotFound), errors.Is(err, ledger.ErrTransactionNotFound):
		code = codes.NotFound
	case errors.Is(err, ledger.ErrInsufficientFunds), errors.Is(err, ledger.ErrCurrencyMismatch):
		code = codes.FailedPrecondition
	case errors.Is(err, ledger.ErrIdempotencyConflict):
		code = codes.AlreadyExists
	default:
		s.log.ErrorContext(ctx, "grpc request failed", "err", err)
		return status.Error(codes.Internal, "internal error")
	}
	return status.Error(code, err.Error())
}

func toAccountPB(a ledger.Account) *ledgerv1.Account {
	return &ledgerv1.Account{
		Id:                   a.ID.String(),
		Name:                 a.Name,
		Currency:             a.Currency,
		AllowNegativeBalance: a.AllowNegativeBalance,
		CreateTime:           timestamppb.New(a.CreatedAt),
	}
}

func toEntryPB(e ledger.Entry) *ledgerv1.Entry {
	return &ledgerv1.Entry{
		Id:            e.ID.String(),
		TransactionId: e.TransactionID.String(),
		AccountId:     e.AccountID.String(),
		Currency:      e.Currency,
		Amount:        e.Amount,
		CreateTime:    timestamppb.New(e.CreatedAt),
	}
}

func toTransactionPB(t ledger.Transaction) *ledgerv1.Transaction {
	entries := make([]*ledgerv1.Entry, len(t.Entries))
	for i, e := range t.Entries {
		entries[i] = toEntryPB(e)
	}
	return &ledgerv1.Transaction{
		Id:             t.ID.String(),
		IdempotencyKey: t.IdempotencyKey,
		Description:    t.Description,
		Entries:        entries,
		CreateTime:     timestamppb.New(t.CreatedAt),
	}
}
