package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"log"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/medonomator/files-service/migrations"
)

func main() {
	command := flag.String("command", "up", "goose command: up, down, status, version")
	flag.Parse()

	if err := run(context.Background(), *command); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, command string) error {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}

	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return err
	}

	defer db.Close()

	goose.SetBaseFS(migrations.FS)

	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}

	return goose.RunContext(ctx, command, db, ".")
}
