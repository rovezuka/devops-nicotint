// worker — фоновый обработчик заказов shop.
// Раз в WORKER_INTERVAL забирает пачку заказов со status='new' и помечает их 'processed'.
// FOR UPDATE SKIP LOCKED позволяет нескольким репликам работать параллельно
// и не обрабатывать один заказ дважды.
//
// На :8080 отдаёт /health (для проб) и /metrics.
package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"shop/internal/db"
)

var (
	processed = promauto.NewCounter(prometheus.CounterOpts{
		Name: "shop_worker_orders_processed_total",
		Help: "Orders marked as processed.",
	})
	errorsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "shop_worker_errors_total",
		Help: "Failed processing iterations (usually db unavailable).",
	})
	pending = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "shop_worker_pending_orders",
		Help: "Orders with status=new seen on the last iteration.",
	})
	lastSuccess = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "shop_worker_last_success_timestamp_seconds",
		Help: "Unix time of the last successful iteration.",
	})
)

const claimBatch = `
UPDATE orders SET status = 'processed', processed_at = now()
WHERE id IN (
    SELECT id FROM orders WHERE status = 'new'
    ORDER BY id LIMIT $1
    FOR UPDATE SKIP LOCKED
)
RETURNING id`

func main() {
	conn := db.Open()
	defer conn.Close()

	interval, err := time.ParseDuration(envOr("WORKER_INTERVAL", "5s"))
	if err != nil {
		log.Fatalf("bad WORKER_INTERVAL: %v", err)
	}
	batch, _ := strconv.Atoi(envOr("WORKER_BATCH", "10"))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		if os.Getenv("HEALTH_FAIL") == "true" {
			http.Error(w, "unhealthy (HEALTH_FAIL=true)", http.StatusInternalServerError)
			return
		}
		w.Write([]byte("ok"))
	})
	mux.Handle("GET /metrics", promhttp.Handler())
	srv := &http.Server{Addr: ":" + envOr("PORT", "8080"), Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	log.Printf("worker started: interval=%s batch=%d", interval, batch)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Println("shutting down")
			srv.Shutdown(context.Background())
			return
		case <-ticker.C:
			tick(ctx, conn, batch)
		}
	}
}

func tick(ctx context.Context, conn *sql.DB, batch int) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	rows, err := conn.QueryContext(ctx, claimBatch, batch)
	if err != nil {
		errorsTotal.Inc()
		log.Printf("claim batch: %v", err)
		return
	}
	n := 0
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		log.Printf("processed order %d", id)
		n++
	}
	rows.Close()
	processed.Add(float64(n))

	var left int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM orders WHERE status = 'new'`).Scan(&left); err != nil {
		errorsTotal.Inc()
		return
	}
	pending.Set(float64(left))
	lastSuccess.SetToCurrentTime()
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
