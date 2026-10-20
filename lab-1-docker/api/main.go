// api — подопытный сервис для Лабы 1.
//
//	GET /health      -> "ok"
//	GET /eat?mb=N    -> выделяет N МБ и держит их до конца жизни процесса
//	GET /burn        -> запускает горутину, крутящую одно ядро в бесконечном цикле
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
)

var (
	mu    sync.Mutex
	hoard [][]byte // держим ссылки, чтобы GC не освободил память
	total int
)

func health(w http.ResponseWriter, _ *http.Request) {
	fmt.Fprintln(w, "ok")
}

func eat(w http.ResponseWriter, r *http.Request) {
	mb, err := strconv.Atoi(r.URL.Query().Get("mb"))
	if err != nil || mb <= 0 {
		http.Error(w, "usage: /eat?mb=N (N > 0)", http.StatusBadRequest)
		return
	}
	buf := make([]byte, mb<<20)
	// Пишем в каждую страницу: иначе ядро не выделит физическую память (RSS не вырастет).
	for i := 0; i < len(buf); i += 4096 {
		buf[i] = 1
	}
	mu.Lock()
	hoard = append(hoard, buf)
	total += mb
	cur := total
	mu.Unlock()
	log.Printf("eat: +%d MB, total %d MB", mb, cur)
	fmt.Fprintf(w, "allocated %d MB, total held %d MB\n", mb, cur)
}

func burn(w http.ResponseWriter, _ *http.Request) {
	go func() {
		for {
		}
	}()
	log.Printf("burn: started busy loop")
	fmt.Fprintln(w, "burning one core")
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	http.HandleFunc("/health", health)
	http.HandleFunc("/eat", eat)
	http.HandleFunc("/burn", burn)
	log.Printf("api listening on :%s (pid %d)", port, os.Getpid())
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

// cache test