// api — stateless HTTP-сервис магазина shop.
//
//	GET  /health  — "ok"; при HEALTH_FAIL=true отвечает 500
//	POST /order   — {"item":"book","qty":2} → создаёт заказ
//	GET  /orders  — список последних заказов
//	GET  /version — версия образа (APP_VERSION), удобно смотреть rolling update
//	GET  /metrics — метрики Prometheus
package main

import (
	"context"
	"database/sql"
	"encoding/json"
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
	version = envOr("APP_VERSION", "dev")

	httpRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "HTTP requests by handler, method and status code.",
	}, []string{"handler", "method", "code"})

	httpDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP request latency.",
		Buckets: prometheus.DefBuckets,
	}, []string{"handler", "method"})

	ordersCreated = promauto.NewCounter(prometheus.CounterOpts{
		Name: "shop_orders_created_total",
		Help: "Orders successfully written to postgres.",
	})
)

type order struct {
	ID          int64      `json:"id"`
	Item        string     `json:"item"`
	Qty         int        `json:"qty"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	ProcessedAt *time.Time `json:"processed_at,omitempty"`
}

func main() {
	conn := db.Open()
	defer conn.Close()

	mux := http.NewServeMux()
	mux.Handle("GET /health", instrument("health", health))
	mux.Handle("GET /version", instrument("version", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(version + "\n"))
	}))
	mux.Handle("POST /order", instrument("order", createOrder(conn)))
	mux.Handle("GET /orders", instrument("orders", listOrders(conn)))
	mux.Handle("GET /metrics", promhttp.Handler())

	srv := &http.Server{Addr: ":" + envOr("PORT", "8080"), Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	// Graceful shutdown: на SIGTERM дорабатываем текущие запросы, а не рвём их.
	// Без этого rolling update теряет запросы в момент остановки старого пода.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	go func() {
		log.Printf("api %s listening on %s", version, srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	<-ctx.Done()
	log.Println("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
}

func health(w http.ResponseWriter, _ *http.Request) {
	if os.Getenv("HEALTH_FAIL") == "true" {
		http.Error(w, "unhealthy (HEALTH_FAIL=true)", http.StatusInternalServerError)
		return
	}
	w.Write([]byte("ok"))
}

func createOrder(conn *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Item string `json:"item"`
			Qty  int    `json:"qty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Item == "" {
			http.Error(w, `body must be {"item":"...","qty":N}`, http.StatusBadRequest)
			return
		}
		if in.Qty <= 0 {
			in.Qty = 1
		}
		var o order
		err := conn.QueryRowContext(r.Context(),
			`INSERT INTO orders (item, qty) VALUES ($1, $2)
			 RETURNING id, item, qty, status, created_at, processed_at`, in.Item, in.Qty).
			Scan(&o.ID, &o.Item, &o.Qty, &o.Status, &o.CreatedAt, &o.ProcessedAt)
		if err != nil {
			log.Printf("insert order: %v", err)
			http.Error(w, "db error", http.StatusServiceUnavailable)
			return
		}
		ordersCreated.Inc()
		writeJSON(w, http.StatusCreated, o)
	}
}

func listOrders(conn *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := conn.QueryContext(r.Context(),
			`SELECT id, item, qty, status, created_at, processed_at
			 FROM orders ORDER BY id DESC LIMIT 100`)
		if err != nil {
			log.Printf("list orders: %v", err)
			http.Error(w, "db error", http.StatusServiceUnavailable)
			return
		}
		defer rows.Close()
		out := []order{}
		for rows.Next() {
			var o order
			if err := rows.Scan(&o.ID, &o.Item, &o.Qty, &o.Status, &o.CreatedAt, &o.ProcessedAt); err != nil {
				http.Error(w, "db error", http.StatusInternalServerError)
				return
			}
			out = append(out, o)
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// instrument считает запросы и латентность — пригодится для алертов в Части 5.
func instrument(name string, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		h(rec, r)
		httpDuration.WithLabelValues(name, r.Method).Observe(time.Since(start).Seconds())
		httpRequests.WithLabelValues(name, r.Method, strconv.Itoa(rec.code)).Inc()
	})
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.code = code
	s.ResponseWriter.WriteHeader(code)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
