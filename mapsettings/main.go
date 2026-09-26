// map-settings — отдельная программа чтения/инспекции/редактирования ячеек МАП.
// Copyright (C) 2026  Aleksandr Galinskii
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// version подставляется через -ldflags "-X main.version=..." (по умолчанию dev).
var version = "dev"

// cfgPath — путь к локальному конфигу (mapsettings.json), сохраняется сервером
// при изменении режима/IP/порта. Файл в .gitignore.
var cfgPath string

func main() {
	var (
		listen  = flag.String("listen", ":8099", "адрес прослушивания HTTP (напр. :8099 или 127.0.0.1:8099)")
		mapAddr = flag.String("map", "192.168.13.60:502", "адрес mapgateway (host:port), по умолчанию для UI")
		unit    = flag.Int("unit", 1, "Modbus-адрес МАП (unit id)")
		user    = flag.String("user", "", "HTTP Basic: логин (пусто — без авторизации)")
		pass    = flag.String("pass", "", "HTTP Basic: пароль")
		showVer = flag.Bool("version", false, "показать версию и выйти")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("map-settings %s\n", version)
		return
	}

	// Какие флаги заданы явно (они приоритетнее конфига).
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })

	cfgPath = configPath()
	fc := loadFileConfig(cfgPath)

	effListen := *listen
	if !set["listen"] && fc != nil && fc.Listen != "" {
		effListen = fc.Listen
	}
	effMap := *mapAddr
	if !set["map"] && fc != nil && fc.Map != "" {
		effMap = fc.Map
	}
	effUnit := *unit
	if !set["unit"] && fc != nil && fc.Unit != 0 {
		effUnit = fc.Unit
	}
	effUser, effPass := *user, *pass
	if !set["user"] && fc != nil && fc.User != "" {
		effUser = fc.User
	}
	if !set["pass"] && fc != nil && fc.Pass != "" {
		effPass = fc.Pass
	}

	ip, port, err := splitHostPort(effMap)
	if err != nil {
		log.Fatalf("map-settings: адрес МАП: %v", err)
	}
	if effUnit <= 0 || effUnit > 255 {
		log.Fatalf("map-settings: некорректный unit %d", effUnit)
	}
	// Режим/IP/порт по умолчанию для UI: из конфига, если сохранены.
	mode := mapModeDominator
	if fc != nil && fc.Mode != "" {
		mode = normalizeMapMode(fc.Mode)
	}
	if fc != nil && fc.IP != "" {
		ip = fc.IP
	}
	if fc != nil && fc.Port != 0 {
		port = fc.Port
	}
	defaults = mapSettingsTarget{mode: mode, ip: ip, port: port, unit: byte(effUnit)}

	mux := newMux(effUser, effPass)
	srv := &http.Server{
		Addr:              effListen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Printf("map-settings %s: http://%s/ (МАП по умолчанию %s:%d, unit %d; конфиг %s)",
		version, effListen, defaults.ip, defaults.port, defaults.unit, cfgPath)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("map-settings: %v", err)
	}
}

// splitHostPort разбирает "host:port"; при отсутствии порта берёт 502.
func splitHostPort(s string) (string, int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", 0, fmt.Errorf("пустой адрес")
	}
	host, portStr, err := net.SplitHostPort(s)
	if err != nil {
		if strings.Contains(s, ":") {
			return "", 0, err
		}
		host, portStr = s, "502"
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return "", 0, fmt.Errorf("некорректный порт %q", portStr)
	}
	return host, port, nil
}

// writeJSON/writeJSONStatus — небольшие помощники ответов API.
func writeJSONStatus(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSON(w http.ResponseWriter, v any) { writeJSONStatus(w, http.StatusOK, v) }

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
