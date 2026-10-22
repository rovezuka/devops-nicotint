// batch — заглушка "ночной аналитики": жрёт CPU и память, ничего полезного не делает.
//
// Настройка через переменные окружения:
//   BATCH_MEMORY_MB   — сколько памяти занять и держать (по умолчанию 64)
//   BATCH_STEP_MB     — каким шагом набирать память (по умолчанию 16)
//   BATCH_STEP_DELAY  — пауза между шагами набора памяти (по умолчанию 2s)
//   BATCH_CPU_WORKERS — сколько горутин крутят CPU (по умолчанию 1)
package main

import (
	"log"
	"os"
	"strconv"
	"time"
)

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return def
}

func envDuration(name string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(name)); err == nil {
		return v
	}
	return def
}

func burn() {
	x := 0
	for {
		x++
	}
}

func main() {
	memMB := envInt("BATCH_MEMORY_MB", 64)
	stepMB := envInt("BATCH_STEP_MB", 16)
	stepDelay := envDuration("BATCH_STEP_DELAY", 2*time.Second)
	workers := envInt("BATCH_CPU_WORKERS", 1)

	log.Printf("batch: memory=%dMB step=%dMB delay=%s cpu_workers=%d", memMB, stepMB, stepDelay, workers)

	for i := 0; i < workers; i++ {
		go burn()
	}

	var hold [][]byte
	for held := 0; held < memMB; held += stepMB {
		chunk := make([]byte, stepMB<<20)
		// Пишем в каждую страницу, иначе ядро не выделит физическую память.
		for j := 0; j < len(chunk); j += 4096 {
			chunk[j] = 1
		}
		hold = append(hold, chunk)
		log.Printf("batch: holding %dMB", held+stepMB)
		time.Sleep(stepDelay)
	}

	for {
		time.Sleep(time.Minute)
		log.Printf("batch: still holding %d chunks", len(hold))
	}
}
