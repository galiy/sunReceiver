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
	"bytes"
	"strings"
	"testing"
)

// Мобильная версия: общий common.js (определяет window.srBindChip/window.srTouchChart)
// должен подключаться ПЕРЕД <script src> скрипта страницы — скрипты страниц
// синхронно вызывают srTouchChart при загрузке, и при обратном порядке touch-жесты
// (щипок-зум, панорама) не привязываются (ReferenceError). Так как JS теперь
// вынесен в отдельные файлы (web/static/js), порядок проверяется по позициям
// тегов <script src> в отрендеренном HTML.
func TestMobileCommonScriptOrder(t *testing.T) {
	data := map[string]any{"active": "home", "flags": dashFlags{ShowMap: true, ShowMeter: true, ShowBMS: true}, "CacheBust": "cafebabe1"}
	cases := []struct {
		name string
		page string // имя шаблона страницы
		js   string // файл JS страницы, идущий после common.js
	}{
		{"dash", "index.html", "/static/js/dashboard.js"},
		{"charts", "charts.html", "/static/js/charts.js"},
		{"energy", "energy.html", "/static/js/energy.js"},
		{"bms", "bms.html", "/static/js/bms.js"},
	}
	const commonSrc = `/static/js/common.js`
	for _, tc := range cases {
		var buf bytes.Buffer
		if err := webTemplates.ExecuteTemplate(&buf, tc.page, data); err != nil {
			t.Fatalf("%s: render: %v", tc.name, err)
		}
		html := buf.String()
		common := strings.Index(html, `<script src="`+commonSrc)
		if common < 0 {
			t.Errorf("%s: common.js не подключён в странице", tc.name)
			continue
		}
		page := strings.Index(html, `<script src="`+tc.js)
		if page < 0 {
			t.Errorf("%s: скрипт страницы %q не найден", tc.name, tc.js)
			continue
		}
		if common > page {
			t.Errorf("%s: common.js (позиция %d) подключён ПОСЛЕ скрипта страницы (%s на %d) — srTouchChart будет undefined при загрузке", tc.name, common, tc.js, page)
		}
	}
}
