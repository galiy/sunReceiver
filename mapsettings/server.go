// map-settings — HTTP-слой отдельной программы.
// Copyright (C) 2026  Aleksandr Galinskii
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"time"
)

//go:embed web
var webFS embed.FS

// defaults — адрес/режим по умолчанию из флагов; клиент может переопределить
// IP/порт/режим (сохраняются в localStorage браузера).
var defaults mapSettingsTarget

// readTimeout — верхняя граница на медленные Modbus-операции с МАП.
const readTimeout = 90 * time.Second

func newMux(user, pass string) http.Handler {
	mux := http.NewServeMux()
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("map-settings: web: %v", err)
	}
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(sub))))
	auth := basicAuth(user, pass)
	mux.Handle("/api/config", auth(http.HandlerFunc(apiConfig)))
	mux.Handle("/api/settings", auth(http.HandlerFunc(apiSettings)))
	mux.Handle("/api/apply", auth(http.HandlerFunc(apiApply)))
	mux.Handle("/api/action", auth(http.HandlerFunc(apiAction)))
	mux.Handle("/api/time", auth(http.HandlerFunc(apiTime)))
	mux.HandleFunc("/", indexHandler)
	return mux
}

func basicAuth(user, pass string) func(http.Handler) http.Handler {
	if user == "" && pass == "" {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, p, ok := r.BasicAuth()
			valid := ok &&
				subtle.ConstantTimeCompare([]byte(u), []byte(user)) == 1 &&
				subtle.ConstantTimeCompare([]byte(p), []byte(pass)) == 1
			if !valid {
				w.Header().Set("WWW-Authenticate", `Basic realm="map-settings"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

// targetReq — адрес/режим, приходящие с клиента.
type targetReq struct {
	Mode string `json:"mode"`
	IP   string `json:"ip"`
	Port int    `json:"port"`
}

func resolveTarget(req targetReq) (mapSettingsTarget, error) {
	mode := req.Mode
	if mode == "" {
		mode = defaults.mode
	}
	ip := req.IP
	if ip == "" {
		ip = defaults.ip
	}
	port := req.Port
	if port == 0 {
		port = defaults.port
	}
	t, err := validateMapTarget(mode, ip, port)
	if err != nil {
		return t, err
	}
	if defaults.unit != 0 {
		t.unit = defaults.unit
	}
	return t, nil
}

func targetFromQuery(r *http.Request) (mapSettingsTarget, error) {
	port := atoiDefault(r.URL.Query().Get("port"), 0)
	return resolveTarget(targetReq{Mode: r.URL.Query().Get("mode"), IP: r.URL.Query().Get("ip"), Port: port})
}

func apiConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var req targetReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		mode := req.Mode
		if mode == "" {
			mode = defaults.mode
		}
		ip := req.IP
		if ip == "" {
			ip = defaults.ip
		}
		port := req.Port
		if port == 0 {
			port = defaults.port
		}
		if _, err := validateMapTarget(mode, ip, port); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defaults.mode = normalizeMapMode(mode)
		defaults.ip = ip
		defaults.port = port
		fc := loadFileConfig(cfgPath)
		if fc == nil {
			fc = &fileConfig{}
		}
		fc.Mode, fc.IP, fc.Port = defaults.mode, ip, port
		if err := saveFileConfig(cfgPath, fc); err != nil {
			log.Printf("map-settings: сохранение конфига %s: %v", cfgPath, err)
		}
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	writeJSON(w, map[string]any{
		"mode": defaults.mode,
		"ip":   defaults.ip,
		"port": defaults.port,
		"unit": int(defaults.unit),
	})
}

func apiSettings(w http.ResponseWriter, r *http.Request) {
	target, err := targetFromQuery(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
	defer cancel()
	snap, err := readMapSettingsSnapshot(ctx, target)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, snap)
}

func apiApply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		targetReq
		Changes map[string]float64 `json:"changes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	target, err := resolveTarget(req.targetReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Changes) == 0 {
		writeJSON(w, map[string]any{"results": map[string]string{}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
	defer cancel()
	results, err := applyMapSettings(ctx, target, req.Changes)
	resp := map[string]any{"results": results}
	if err != nil {
		resp["error"] = err.Error()
		log.Printf("map-settings: apply %s: %v", target.address(), err)
		writeJSONStatus(w, http.StatusBadGateway, resp)
		return
	}
	log.Printf("map-settings: apply %s: изменено %d параметр(ов)", target.address(), len(req.Changes))
	writeJSON(w, resp)
}

func apiAction(w http.ResponseWriter, r *http.Request) {
	var req struct {
		targetReq
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	target, err := resolveTarget(req.targetReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
	defer cancel()
	if err := runMapSettingsAction(ctx, target, req.Key); err != nil {
		log.Printf("map-settings: action %q %s: %v", req.Key, target.address(), err)
		writeJSONStatus(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	log.Printf("map-settings: action %q %s: выполнено", req.Key, target.address())
	writeJSON(w, map[string]any{"ok": true})
}

func apiTime(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		target, err := targetFromQuery(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
		defer cancel()
		st, err := readMapTime(ctx, target)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, st)
		return
	}
	var req struct {
		targetReq
		Hour   int `json:"hour"`
		Minute int `json:"minute"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	target, err := resolveTarget(req.targetReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
	defer cancel()
	if err := writeMapTime(ctx, target, req.Hour, req.Minute); err != nil {
		log.Printf("map-settings: time %s: %v", target.address(), err)
		writeJSONStatus(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	log.Printf("map-settings: time %s: %02d:%02d записано", target.address(), req.Hour, req.Minute)
	writeJSON(w, map[string]any{"ok": true})
}

// strconv используется в helpers main.go; оставлено для единообразия импорта.
