// sunReceiver
// Copyright (C) 2026  Aleksandr Galinskii
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"context"
	"log"
	"net/http"
	_ "net/http/pprof"
	"runtime"
	"time"
)

// Диагностика процесса: периодический снимок heap/goroutine в журнал и pprof
// только на loopback. Нужна для разбора утечек (AGM/BLE), из-за которых сервис
// ранее доходил до OOM за ~9–11 часов.

const (
	memStatsInterval = 5 * time.Minute
	pprofAddr        = "127.0.0.1:6060"
)

// runMemStatsLogger пишет компактный снимок памяти и числа горутин. Рост
// HeapAlloc/NumGoroutine между строками — признак утечки.
func runMemStatsLogger(ctx context.Context) {
	t := time.NewTicker(memStatsInterval)
	defer t.Stop()
	logMemStats()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			logMemStats()
		}
	}
}

func logMemStats() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	log.Printf("diag: heap=%d MiB inuse=%d MiB idle=%d MiB released=%d MiB sys=%d MiB stack=%d MiB goroutines=%d gc=%d",
		m.HeapAlloc>>20, m.HeapInuse>>20, m.HeapIdle>>20, m.HeapReleased>>20,
		m.Sys>>20, m.StackInuse>>20, runtime.NumGoroutine(), m.NumGC)
}

// runPprofServer поднимает /debug/pprof только на 127.0.0.1 (снаружи недоступен).
// Профили снимаются с dev-ПК через SSH-туннель к 6060.
func runPprofServer(ctx context.Context) {
	srv := &http.Server{Addr: pprofAddr, Handler: http.DefaultServeMux}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("diag: pprof %s: %v", pprofAddr, err)
		}
	}()
	log.Printf("diag: pprof на http://%s/debug/pprof/ (только loopback)", pprofAddr)
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
