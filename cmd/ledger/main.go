// Command ledger serves the ledger over HTTP and gRPC.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	ledgerv1 "github.com/UncleKweks/ledger-service/gen/ledger/v1"
	"github.com/UncleKweks/ledger-service/internal/grpcapi"
	"github.com/UncleKweks/ledger-service/internal/httpapi"
	"github.com/UncleKweks/ledger-service/internal/ledger"
	"github.com/UncleKweks/ledger-service/internal/store/postgres"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("ledger stopped", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, env("DATABASE_URL", "postgres://ledger:ledger_dev@127.0.0.1:55432/ledger?sslmode=disable"))
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return err
	}
	log.Info("connected to database")

	svc := ledger.NewService(postgres.New(pool))

	httpSrv := &http.Server{
		Addr:              env("HTTP_ADDR", ":8080"),
		Handler:           httpapi.NewHandler(svc, log),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	grpcSrv := grpc.NewServer()
	ledgerv1.RegisterLedgerServiceServer(grpcSrv, grpcapi.NewServer(svc, log))
	healthpb.RegisterHealthServer(grpcSrv, health.NewServer())
	reflection.Register(grpcSrv)
	grpcAddr := env("GRPC_ADDR", ":9090")
	grpcLis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		return err
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		log.Info("http listening", "addr", httpSrv.Addr)
		if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	g.Go(func() error {
		log.Info("grpc listening", "addr", grpcAddr)
		return grpcSrv.Serve(grpcLis)
	})
	// Shut both servers down on a signal or when either one fails.
	g.Go(func() error {
		<-gctx.Done()
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		grpcSrv.GracefulStop()
		return httpSrv.Shutdown(shutdownCtx)
	})
	return g.Wait()
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
