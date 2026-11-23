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
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// garagePage — страница модуля «Управление гаражом» (/garage): состояние
// модуля и реле, значимые входные данные мониторинга, график состояний реле.
// Доступна и при выключенном модуле (показывает историю журнала).
func (h *dashboardHandler) garagePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := webTemplates.ExecuteTemplate(w, "garage.html", map[string]any{"active": "garage", "flags": h.flags, "CacheBust": webCacheBust}); err != nil {
		log.Printf("dashboard: render /garage: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// garageAPISnapshot — ответ /api/garage.
type garageAPISnapshot struct {
	garageSnapshot
	Enabled    bool   `json:"enabled"`
	DTS017     bool   `json:"dts017_enabled"`
	ServerTime string `json:"server_time"`
}

// apiGarage — GET /api/garage: текущее состояние модуля гаража и входные данные.
func (h *dashboardHandler) apiGarage(w http.ResponseWriter, r *http.Request) {
	snap := garageMod.GarageSnapshot()
	resp := garageAPISnapshot{
		garageSnapshot: snap,
		Enabled:        h.flags.ShowGarage,
		DTS017:         h.flags.ShowDTS017,
		ServerTime:     time.Now().Format(time.RFC3339),
	}
	if resp.Inverters == nil {
		resp.Inverters = []garageInverterView{}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("dashboard: /api/garage: %v", err)
	}
}

// apiGarageEvents — GET /api/garage/events?from&to: журнал переключений реле из PG.
// По умолчанию — последние 24 часа.
func (h *dashboardHandler) apiGarageEvents(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	to := now
	if s := r.URL.Query().Get("to"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			to = t
		}
	}
	from := to.Add(-24 * time.Hour)
	if s := r.URL.Query().Get("from"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			from = t
		}
	}
	if to.After(now) {
		to = now
	}
	if !from.Before(to) {
		http.Error(w, "некорректный период", http.StatusBadRequest)
		return
	}
	out := struct {
		Events       []garageRelayEvent `json:"events"`
		Initial      [4]bool            `json:"initial"`
		InitialKnown [4]bool            `json:"initial_known"`
		JournalStart *time.Time         `json:"journal_start"`
		Current      [4]bool            `json:"current"`
		Usage        garageUsageStats   `json:"usage"`
	}{Events: []garageRelayEvent{}}
	if h.pg != nil {
		events, err := h.pg.garageRelayEvents(from, to)
		if err != nil {
			log.Printf("dashboard: /api/garage/events: %v", err)
			http.Error(w, "ошибка чтения журнала", http.StatusInternalServerError)
			return
		}
		if events != nil {
			out.Events = events
		}
		init, known, err := h.pg.garageRelayStatesAt(from)
		if err != nil {
			log.Printf("dashboard: /api/garage/events initial: %v", err)
		}
		out.Initial = init
		out.InitialKnown = known
		if js, err := h.pg.garageJournalStart(); err != nil {
			log.Printf("dashboard: /api/garage/events journal start: %v", err)
		} else {
			out.JournalStart = js
		}
		out.Current = garageMod.GarageSnapshot().Relays
		out.Usage = h.garageStabilizerUsage(now)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		log.Printf("dashboard: /api/garage/events encode: %v", err)
	}
}

// garageUsageStats — накопленное время в режиме «через стабилизатор» (реле 3
// в off) за текущие сутки/месяц MSK и оценка расхода по stabilizer_power_w.
type garageUsageStats struct {
	StabilizerPowerW float64 `json:"stabilizer_power_w"`
	DaySeconds       float64 `json:"day_stabilizer_sec"`
	DayKWh           float64 `json:"day_stabilizer_kwh"`
	MonthSeconds     float64 `json:"month_stabilizer_sec"`
	MonthKWh         float64 `json:"month_stabilizer_kwh"`
}

// garageStabilizerUsage считает время работы стабилизатора из журнала реле.
// Реле 3 выключено = подача через стабилизатор.
func (h *dashboardHandler) garageStabilizerUsage(now time.Time) garageUsageStats {
	var st garageUsageStats
	if garageCfg != nil {
		st.StabilizerPowerW = garageCfg.StabilizerPowerW
	}
	if h.pg == nil {
		return st
	}
	dayStart := deviceErrDayStart(now)
	y, m, _ := now.In(deviceErrMSK).Date()
	monthStart := time.Date(y, m, 1, 0, 0, 0, 0, deviceErrMSK).UTC()
	if d, err := h.pg.garageRelayStateDuration(3, false, dayStart, now); err != nil {
		log.Printf("dashboard: /api/garage/events: стабилизатор за сутки: %v", err)
	} else {
		st.DaySeconds = d.Seconds()
		st.DayKWh = round2(d.Hours() * st.StabilizerPowerW / 1000)
	}
	if d, err := h.pg.garageRelayStateDuration(3, false, monthStart, now); err != nil {
		log.Printf("dashboard: /api/garage/events: стабилизатор за месяц: %v", err)
	} else {
		st.MonthSeconds = d.Seconds()
		st.MonthKWh = round2(d.Hours() * st.StabilizerPowerW / 1000)
	}
	return st
}
