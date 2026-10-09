// Command migrate applies the embedded SQL migrations with goose.
//
//	go run ./cmd/migrate up | down | status | reset
package main

import (
	"database/sql"
	"log"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/UncleKweks/ledger-service/migrations"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: migrate up|down|status|reset")
	}

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://ledger:ledger_dev@localhost:5433/ledger?sslmode=disable"
	}

	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		log.Fatal("cannot open database: ", err)
	}
	defer db.Close()

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		log.Fatal(err)
	}

	if err := goose.Run(os.Args[1], db, "."); err != nil {
		log.Fatal(err)
	}
}
