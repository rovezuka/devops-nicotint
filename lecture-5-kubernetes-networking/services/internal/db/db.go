// Package db — общее подключение к postgres и схема для api и worker.
package db

import (
	"context"
	"database/sql"
	"log"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const schema = `
CREATE TABLE IF NOT EXISTS orders (
    id           BIGSERIAL PRIMARY KEY,
    item         TEXT        NOT NULL,
    qty          INT         NOT NULL DEFAULT 1,
    status       TEXT        NOT NULL DEFAULT 'new',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at TIMESTAMPTZ
);`

// Open возвращает пул соединений. sql.Open ленивый — сервис стартует,
// даже если postgres ещё не поднялся, а схема создаётся в фоне с ретраями.
func Open() *sql.DB {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://app:app@localhost:5432/app?sslmode=disable"
	}
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Fatalf("db: invalid DATABASE_URL: %v", err)
	}
	conn.SetMaxOpenConns(10)
	conn.SetConnMaxIdleTime(time.Minute)

	go func() {
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, err := conn.ExecContext(ctx, schema)
			cancel()
			if err == nil {
				log.Println("db: schema ready")
				return
			}
			log.Printf("db: schema not ready, retrying: %v", err)
			time.Sleep(3 * time.Second)
		}
	}()
	return conn
}
