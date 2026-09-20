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
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestDashboardAPIRouting — дымовой тест HTTP-роутинга дашборда. Ловит регрессию,
// когда `/api/*` монтировался без http.StripPrefix и весь API отдавал 404:
// вложенный подмьютекс получал путь `/api/current` вместо `/current`.
func TestDashboardAPIRouting(t *testing.T) {
	hit := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(name))
		}
	}
	pages := map[string]http.HandlerFunc{
		"/":       hit("index"),
		"/charts": hit("charts"),
		"/energy": hit("energy"),
		"/bms/":   hit("bmsDetail"),
	}
	api := map[string]http.HandlerFunc{
		"/current": hit("current"),
		"/series":  hit("series"),
		"/tariffs": hit("tariffs"),
		"/bms":     hit("bms"),
		"/bms/":    hit("bmsOne"),
		"/relay":   hit("relay"),
	}

	// Без учётных данных API закрыт (401), но роутинг существует (не 404).
	mux := buildDashboardMux(pages, http.NotFoundHandler(), api, "u", "p")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/current", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("anon /api/current → %d, want 401", rr.Code)
	}

	authGet := func(path string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.SetBasicAuth("u", "p")
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		return rr.Code
	}

	// С верными креденшелами хендлер вызывается (200): префикс /api/ срезан.
	if code := authGet("/api/current"); code != http.StatusOK {
		t.Fatalf("auth /api/current → %d, want 200 (StripPrefix сломан?)", code)
	}
	if code := authGet("/api/series"); code != http.StatusOK {
		t.Fatalf("auth /api/series → %d, want 200", code)
	}
	if code := authGet("/api/tariffs"); code != http.StatusOK {
		t.Fatalf("auth /api/tariffs → %d, want 200", code)
	}
	if code := authGet("/api/bms"); code != http.StatusOK {
		t.Fatalf("auth /api/bms → %d, want 200", code)
	}
	if code := authGet("/api/bms/foo"); code != http.StatusOK {
		t.Fatalf("auth /api/bms/foo → %d, want 200", code)
	}
	if code := authGet("/api/bms/foo/series"); code != http.StatusOK {
		t.Fatalf("auth /api/bms/foo/series → %d, want 200", code)
	}
	if code := authGet("/api/relay"); code != http.StatusOK {
		t.Fatalf("auth /api/relay → %d, want 200", code)
	}

	// Страницы открыты без авторизации — требовать auth на "/" нельзя.
	if code := authGet("/"); code != http.StatusOK {
		t.Fatalf("GET / → %d, want 200 (страница не должна требовать auth)", code)
	}
}