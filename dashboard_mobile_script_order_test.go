package main

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
)

// Мобильная версия: mjs (определяет window.srBindChip/window.srTouchChart)
// должен рендериться ПЕРЕД inline-скриптом страницы — скрипты страниц
// синхронно вызывают srTouchChart при загрузке, и при обратном порядке
// touch-жесты (щипок-зум, панорама) не привязываются (ReferenceError).
func TestMobileMjsScriptOrder(t *testing.T) {
	data := map[string]any{"active": "home", "flags": dashFlags{ShowMap: true, ShowMeter: true, ShowBMS: true}}
	cases := []struct {
		name   string
		tmpl   *template.Template
		marker string // маркер inline-скрипта страницы, идущего после mjs
	}{
		{"dash", dashboardTmpl, "setInterval(tick,1000)"},
		{"charts", chartsTmpl, "function renderChart(id, datasets, opts)"},
		{"energy", energyTmpl, "var TARIFF_COLORS"},
		{"bms", bmsDetailTmpl, "function fmtNum(n,digits)"},
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		if err := tc.tmpl.Execute(&buf, data); err != nil {
			t.Fatalf("%s: render: %v", tc.name, err)
		}
		html := buf.String()
		def := strings.Index(html, "window.srBindChip = function")
		if def < 0 {
			t.Errorf("%s: mjs-скрипт не найден в странице", tc.name)
			continue
		}
		marker := strings.Index(html, tc.marker)
		if marker < 0 {
			t.Errorf("%s: маркер скрипта страницы %q не найден", tc.name, tc.marker)
			continue
		}
		if def > marker {
			t.Errorf("%s: mjs (позиция %d) рендерится ПОСЛЕ скрипта страницы (маркер на %d) — srTouchChart будет undefined при загрузке", tc.name, def, marker)
		}
	}
	// srTouchChart определяется до синхронного вызова на load (charts + bms).
	checkCall := func(page string, tmpl *template.Template, call string) {
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, data); err != nil {
			t.Fatalf("%s: render: %v", page, err)
		}
		html := buf.String()
		def := strings.Index(html, "window.srTouchChart = function")
		callPos := strings.Index(html, call)
		if def < 0 || callPos < 0 {
			t.Errorf("%s: не найдены определение srTouchChart (pos %d) и вызов при загрузке (pos %d)", page, def, callPos)
			return
		}
		if def > callPos {
			t.Errorf("%s: вызов srTouchChart при загрузке (pos %d) идёт ДО его определения (pos %d)", page, callPos, def)
		}
	}
	checkCall("charts", chartsTmpl, "srTouchChart(function(){ return window[id]; }")
	checkCall("bms", bmsDetailTmpl, "srTouchChart(function(){ return BMS_CHARTS[id]; }")
}
