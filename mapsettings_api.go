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

// mapsettings_api.go — HTTP API модуля «Настройки МАП» (страница /map-settings).
//
// Чтение и запись выполняются по Modbus TCP к mapgateway (:502 на ПАК «Малина»).
// Адрес/порт/режим приходят с клиента (сохраняются в localStorage браузера).

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// mapSettingsReadTimeout — верхняя граница на медленные Modbus-операции с МАП
// (несколько блоков чтения; mapgateway может отвечать не мгновенно).
const mapSettingsReadTimeout = 90 * time.Second

// mapSettingsPage — HTML-страница «Настройки МАП» (в стиле дашборда sunReceiver).
func (h *dashboardHandler) mapSettingsPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	d := h.mapDefaultsOr()
	data := map[string]any{
		"active":    "mapsettings",
		"flags":     h.flags,
		"CacheBust": webCacheBust,
		"MapMode":   d.mode,
		"MapIP":     d.ip,
		"MapPort":   d.port,
	}
	if err := webTemplates.ExecuteTemplate(w, "mapsettings.html", data); err != nil {
		log.Printf("dashboard: render /map-settings: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// targetReq — общий блок адреса/режима в запросах API.
type targetReq struct {
	Mode string `json:"mode"`
	IP   string `json:"ip"`
	Port int    `json:"port"`
}

func (h *dashboardHandler) mapDefaultsOr() mapSettingsTarget {
	d := h.mapDefaults
	if d.port == 0 {
		d.port = 502
	}
	if d.unit == 0 {
		d.unit = 1
	}
	if d.mode == "" {
		d.mode = mapModeDominator
	}
	return d
}

// resolveMapTarget подставляет дефолты и валидирует адрес.
func (h *dashboardHandler) resolveMapTarget(req targetReq) (mapSettingsTarget, error) {
	d := h.mapDefaultsOr()
	mode := req.Mode
	if mode == "" {
		mode = d.mode
	}
	ip := req.IP
	if ip == "" {
		ip = d.ip
	}
	port := req.Port
	if port == 0 {
		port = d.port
	}
	return validateMapTarget(mode, ip, port)
}

// apiMapSettings — GET /api/map-settings: полный снимок (настройки rw +
// не-настроечные ro) для выбранного режима и адреса.
func (h *dashboardHandler) apiMapSettings(w http.ResponseWriter, r *http.Request) {
	target, err := h.resolveMapTarget(targetReq{
		Mode: r.URL.Query().Get("mode"),
		IP:   r.URL.Query().Get("ip"),
		Port: atoiDefault(r.URL.Query().Get("port"), 0),
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), mapSettingsReadTimeout)
	defer cancel()
	snap, err := readMapSettingsSnapshot(ctx, target)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, snap)
}

// apiMapSettingsApply — POST /api/map-settings/apply: записать только изменённые
// параметры. Тело: {mode, ip, port, changes:{key: value}}.
func (h *dashboardHandler) apiMapSettingsApply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		targetReq
		Changes map[string]float64 `json:"changes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	target, err := h.resolveMapTarget(req.targetReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Changes) == 0 {
		writeJSON(w, map[string]any{"results": map[string]string{}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), mapSettingsReadTimeout)
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

// apiMapSettingsAction — POST /api/map-settings/action: управляющее воздействие.
func (h *dashboardHandler) apiMapSettingsAction(w http.ResponseWriter, r *http.Request) {
	var req struct {
		targetReq
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	target, err := h.resolveMapTarget(req.targetReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), mapSettingsReadTimeout)
	defer cancel()
	if err := runMapSettingsAction(ctx, target, req.Key); err != nil {
		log.Printf("map-settings: action %q %s: %v", req.Key, target.address(), err)
		writeJSONStatus(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	log.Printf("map-settings: action %q %s: выполнено", req.Key, target.address())
	writeJSON(w, map[string]any{"ok": true})
}

// apiMapSettingsTime — GET/POST /api/map-settings/time: отдельная форма времени.
func (h *dashboardHandler) apiMapSettingsTime(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		target, err := h.resolveMapTarget(targetReq{
			Mode: r.URL.Query().Get("mode"),
			IP:   r.URL.Query().Get("ip"),
			Port: atoiDefault(r.URL.Query().Get("port"), 0),
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), mapSettingsReadTimeout)
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
	target, err := h.resolveMapTarget(req.targetReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), mapSettingsReadTimeout)
	defer cancel()
	if err := writeMapTime(ctx, target, req.Hour, req.Minute); err != nil {
		log.Printf("map-settings: time %s: %v", target.address(), err)
		writeJSONStatus(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	log.Printf("map-settings: time %s: %02d:%02d записано", target.address(), req.Hour, req.Minute)
	writeJSON(w, map[string]any{"ok": true})
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return def
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func writeJSON(w http.ResponseWriter, v any) {
	writeJSONStatus(w, http.StatusOK, v)
}

func writeJSONStatus(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
