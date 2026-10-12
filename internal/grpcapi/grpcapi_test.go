package grpcapi_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	ledgerv1 "github.com/UncleKweks/ledger-service/gen/ledger/v1"
	"github.com/UncleKweks/ledger-service/internal/grpcapi"
	"github.com/UncleKweks/ledger-service/internal/ledger"
	"github.com/UncleKweks/ledger-service/internal/store/postgres"
	"github.com/UncleKweks/ledger-service/internal/testdb"
)

// newClient serves the ledger over an in-memory listener and returns a client.
func newClient(t *testing.T) ledgerv1.LedgerServiceClient {
	t.Helper()
	svc := ledger.NewService(postgres.New(testdb.New(t)))
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	ledgerv1.RegisterLedgerServiceServer(srv, grpcapi.NewServer(svc, log))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return ledgerv1.NewLedgerServiceClient(conn)
}

func createAccount(t *testing.T, c ledgerv1.LedgerServiceClient, name string, allowNegative bool) string {
	t.Helper()
	resp, err := c.CreateAccount(context.Background(), &ledgerv1.CreateAccountRequest{
		Name: name, Currency: "NGN", AllowNegativeBalance: allowNegative,
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	return resp.GetAccount().GetId()
}

func transfer(key, from, to string, amount int64) *ledgerv1.PostTransactionRequest {
	return &ledgerv1.PostTransactionRequest{
		IdempotencyKey: key,
		Description:    "transfer",
		Entries: []*ledgerv1.PostTransactionRequest_EntryInput{
			{AccountId: from, Currency: "NGN", Amount: -amount},
			{AccountId: to, Currency: "NGN", Amount: amount},
		},
	}
}

func TestPostTransactionAndReplay(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	world := createAccount(t, c, "world", true)
	alice := createAccount(t, c, "alice", false)

	first, err := c.PostTransaction(ctx, transfer("grpc-1", world, alice, 3_000))
	if err != nil {
		t.Fatalf("PostTransaction: %v", err)
	}
	if first.GetReplayed() {
		t.Error("first post marked replayed")
	}

	again, err := c.PostTransaction(ctx, transfer("grpc-1", world, alice, 3_000))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !again.GetReplayed() || again.GetTransaction().GetId() != first.GetTransaction().GetId() {
		t.Errorf("replay = %v / %s, want replayed original %s",
			again.GetReplayed(), again.GetTransaction().GetId(), first.GetTransaction().GetId())
	}

	bal, err := c.GetBalance(ctx, &ledgerv1.GetBalanceRequest{AccountId: alice})
	if err != nil || bal.GetBalance() != 3_000 {
		t.Fatalf("GetBalance = %v, %v; want 3000", bal.GetBalance(), err)
	}

	got, err := c.GetTransaction(ctx, &ledgerv1.GetTransactionRequest{Id: first.GetTransaction().GetId()})
	if err != nil || len(got.GetTransaction().GetEntries()) != 2 {
		t.Fatalf("GetTransaction = %v, %v", got, err)
	}

	list, err := c.ListEntries(ctx, &ledgerv1.ListEntriesRequest{AccountId: alice})
	if err != nil || len(list.GetEntries()) != 1 {
		t.Fatalf("ListEntries = %d entries, %v; want 1", len(list.GetEntries()), err)
	}
}

func TestStatusCodes(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	world := createAccount(t, c, "world", true)
	alice := createAccount(t, c, "alice", false)
	if _, err := c.PostTransaction(ctx, transfer("fund", world, alice, 100)); err != nil {
		t.Fatalf("fund: %v", err)
	}

	unbalanced := transfer("u", world, alice, 10)
	unbalanced.Entries[1].Amount = 9

	tests := []struct {
		name string
		call func() error
		want codes.Code
	}{
		{"unbalanced", func() error { _, err := c.PostTransaction(ctx, unbalanced); return err }, codes.InvalidArgument},
		{"bad account id", func() error { _, err := c.PostTransaction(ctx, transfer("b", "nope", alice, 1)); return err }, codes.InvalidArgument},
		{"insufficient funds", func() error { _, err := c.PostTransaction(ctx, transfer("i", alice, world, 101)); return err }, codes.FailedPrecondition},
		{"idempotency conflict", func() error { _, err := c.PostTransaction(ctx, transfer("fund", world, alice, 5)); return err }, codes.AlreadyExists},
		{"account not found", func() error {
			_, err := c.GetAccount(ctx, &ledgerv1.GetAccountRequest{Id: "00000000-0000-0000-0000-000000000001"})
			return err
		}, codes.NotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := status.Code(tt.call()); got != tt.want {
				t.Errorf("code = %v, want %v", got, tt.want)
			}
		})
	}
}
