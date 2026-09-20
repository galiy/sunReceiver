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
	"embed"
	"html/template"
	"io/fs"
	"net/http"
)

// webFS — статика и HTML-шаблоны дашборда, эмбедed в бинарник. Вынесены из
// dashboard.go (см. plans/dashboard-refactor-plan.md): JS/CSS/HTML-оболочки не
// лежат в исходнике Go отдельными строками, а подключаются ссылками на /static/.
// На проде отдельных файлов на диске нет — всё живёт внутри исполняемого файла.
//
//go:embed web
var webFS embed.FS

// webTemplates — набор HTML-шаблонов дашборда. Каждая страница — свой файл
// (index/charts/energy/bms.html), все собраны в один набор вместе с base.html,
// который определяет общие <head> и нижнюю навигацию mnav. Рендер — через
// ExecuteTemplate по имени файла страницы.
var webTemplates = template.Must(template.ParseFS(webFS,
	"web/templates/base.html",
	"web/templates/index.html",
	"web/templates/charts.html",
	"web/templates/energy.html",
	"web/templates/bms.html",
))

// staticFiles отдаёт файлы из web/static по префиксу /static/ с длинным
// Cache-Control: контент меняется только между релизами (сборками), поэтому
// браузеры кэшируют его без перепроверок (immutable) и не дёргают reverse proxy.
func staticFiles() http.Handler {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		// http.FS(web) срезает ведущий "/", поэтому путь /static/css/site.css
		// открывается как static/css/site.css внутри каталога web/ — StripPrefix
		// не нужен (иначе путь терял бы префикс static/).
		files.ServeHTTP(w, r)
	})
}
