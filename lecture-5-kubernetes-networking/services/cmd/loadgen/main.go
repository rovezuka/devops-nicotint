// loadgen — клиент-нагрузчик для Лабы 5.
// Непрерывно ходит в api по DNS-имени Service (не по IP пода) и считает результат каждого запроса.
//
//	TARGET       — базовый URL, по умолчанию http://shop-api.shop.svc
//	RPS          — запросов в секунду (по умолчанию 10)
//	TIMEOUT      — таймаут одного запроса (по умолчанию 2s)
//
// Метрики на :8080/metrics:
//
//	loadgen_requests_total{path, result="ok"|"error", code}
//	loadgen_request_duration_seconds{path}
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	requests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "loadgen_requests_total",
		Help: "Client-side requests to api by path, result (ok/error) and HTTP code (0 = no response).",
	}, []string{"path", "result", "code"})

	duration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "loadgen_request_duration_seconds",
		Help:    "Client-side request latency.",
		Buckets: prometheus.DefBuckets,
	}, []string{"path"})
)

func main() {
	target := envOr("TARGET", "http://shop-api.shop.svc")
	rps, _ := strconv.Atoi(envOr("RPS", "10"))
	if rps <= 0 {
		rps = 10
	}
	timeout, err := time.ParseDuration(envOr("TIMEOUT", "2s"))
	if err != nil {
		log.Fatalf("bad TIMEOUT: %v", err)
	}

	// Без keep-alive: каждый запрос — новое соединение: заново DNS-резолв имени Service (Go-резолвер не кэширует)
	// и новый выбор пода правилами kube-proxy. Так хаос подов виден честно, а не маскируется одним долгим соединением.
	client := &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{DisableKeepAlives: true},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.Handle("GET /metrics", promhttp.Handler())
	go func() {
		if err := http.ListenAndServe(":"+envOr("PORT", "8080"), mux); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	log.Printf("loadgen: target=%s rps=%d timeout=%s", target, rps, timeout)

	var seq, okN, errN atomic.Int64
	go func() { // раз в 10 с — сводка в лог
		for range time.Tick(10 * time.Second) {
			log.Printf("ok=%d error=%d", okN.Load(), errN.Load())
		}
	}()

	ticker := time.NewTicker(time.Second / time.Duration(rps))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n := seq.Add(1)
			go func() {
				var ok bool
				if n%5 == 0 { // каждый 5-й — создание заказа, остальные — чтение списка
					ok = do(client, "POST", target+"/order", "/order", fmt.Sprintf(`{"item":"loadgen-%d","qty":1}`, n))
				} else {
					ok = do(client, "GET", target+"/orders", "/orders", "")
				}
				if ok {
					okN.Add(1)
				} else {
					errN.Add(1)
				}
			}()
		}
	}
}

func do(c *http.Client, method, url, path, body string) bool {
	start := time.Now()
	req, _ := http.NewRequest(method, url, bytes.NewBufferString(body))
	resp, err := c.Do(req)
	duration.WithLabelValues(path).Observe(time.Since(start).Seconds())
	if err != nil {
		requests.WithLabelValues(path, "error", "0").Inc()
		log.Printf("%s %s: %v", method, path, err)
		return false
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	code := strconv.Itoa(resp.StatusCode)
	if resp.StatusCode >= 500 {
		requests.WithLabelValues(path, "error", code).Inc()
		log.Printf("%s %s: HTTP %d", method, path, resp.StatusCode)
		return false
	}
	requests.WithLabelValues(path, "ok", code).Inc()
	return true
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
