// maxbot
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
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// normalizeText приводит текст к виду для сравнения ответов: нижний регистр,
// одиночные пробелы, без краевых пробелов.
func normalizeText(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// similarity возвращает нормированное сходство двух строк (1 — идентичны) по
// расстоянию Левенштейна на рунах. Используется для детекта повторяющихся
// финальных ответов агента.
func similarity(a, b string) float64 {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 && len(rb) == 0 {
		return 1
	}
	if len(ra) == 0 || len(rb) == 0 {
		return 0
	}
	if len(ra) < len(rb) {
		ra, rb = rb, ra
	}
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			del := prev[j] + 1
			ins := cur[j-1] + 1
			sub := prev[j-1] + cost
			m := del
			if ins < m {
				m = ins
			}
			if sub < m {
				m = sub
			}
			cur[j] = m
		}
		prev, cur = cur, prev
	}
	return 1 - float64(prev[len(rb)])/float64(len(ra))
}

// modelExpired — истекло ли окно выбранной модели (последняя активность +
// ttl). Модель по умолчанию не истекает.
func modelExpired(st dshState, now time.Time, ttl time.Duration, defProvider, defModel string) bool {
	if st.Provider == "" || st.Model == "" {
		return false
	}
	if st.Provider == defProvider && st.Model == defModel {
		return false
	}
	last := st.ModelLastAPIAt
	if st.ModelSelectedAt > last {
		last = st.ModelSelectedAt
	}
	if last == 0 {
		return false
	}
	return now.Sub(time.UnixMilli(last)) >= ttl
}

// nextAnswerStreak обновляет серию последних ответов: короткий ответ (меньше
// 80 символов) рвёт серию, длинный — добавляется (храним последние 3).
func nextAnswerStreak(prev []string, text string) []string {
	norm := normalizeText(text)
	if len([]rune(norm)) < 80 {
		return nil
	}
	out := append(append([]string(nil), prev...), norm)
	if len(out) > 3 {
		out = out[len(out)-3:]
	}
	return out
}

// nextPromptStreak обновляет серию последних запросов (последние 3).
func nextPromptStreak(prev []string, prompt string) []string {
	p := normalizeText(prompt)
	if p == "" {
		return nil
	}
	out := append(append([]string(nil), prev...), p)
	if len(out) > 3 {
		out = out[len(out)-3:]
	}
	return out
}

// samePrompts — три последних запроса идентичны (нормализованно).
func samePrompts(prompts []string) bool {
	return len(prompts) == 3 && prompts[0] == prompts[1] && prompts[1] == prompts[2]
}

// shouldReplaceAnswers — три последних нормализованных ответа подряд похожи
// (>= 90%). Проверяются ровно последние три элемента.
func shouldReplaceAnswers(answers []string) bool {
	if len(answers) < 3 {
		return false
	}
	a := answers[len(answers)-3:]
	return similarity(a[2], a[1]) >= 0.9 && similarity(a[1], a[0]) >= 0.9
}

// toolFingerprint — отпечаток tool/call: имя + канонический JSON аргументов
// (ключи сортируются, пробелы нормализуются). Если аргументы не JSON —
// используется исходная строка без краевых пробелов.
func toolFingerprint(name, arguments string) string {
	var v any
	if err := json.Unmarshal([]byte(arguments), &v); err == nil {
		if b, err := json.Marshal(v); err == nil {
			return name + "|" + string(b)
		}
	}
	return name + "|" + strings.TrimSpace(arguments)
}

// detectCycle ищет повторяющийся цикл в последовательности отпечатков вызовов:
// период L=1..6, блок должен повториться 3 раза подряд.
func detectCycle(seq []string) (int, bool) {
	n := len(seq)
	for l := 1; l <= 6; l++ {
		if n < 3*l {
			continue
		}
		ok := true
		for i := 0; i < l && ok; i++ {
			last := seq[n-1-i]
			if seq[n-1-i-l] != last || seq[n-1-i-2*l] != last {
				ok = false
			}
		}
		if ok {
			return l, true
		}
	}
	return 0, false
}

// trimToolSeq ограничивает историю отпечатков (нужно не более 18 для детекта,
// храним с запасом).
func trimToolSeq(seq []string, max int) []string {
	if len(seq) <= max {
		return seq
	}
	return append([]string(nil), seq[len(seq)-max:]...)
}

var markdownLine = regexp.MustCompile("(?m)^\\s{0,3}(#{1,6}\\s|```|\\|.*\\||[-*+]\\s|\\d+\\.\\s|>\\s)|\\*\\*[^*]+\\*\\*")
var markdownLink = regexp.MustCompile(`\[[^\]]+\]\([^)]+\)`)

// looksLikeMarkdown — грубая эвристика для выбора расширения файла с длинным
// ответом: заголовки, списки, код-фенсы, таблицы, ссылки, жирный текст.
func looksLikeMarkdown(s string) bool {
	return markdownLine.MatchString(s) || markdownLink.MatchString(s)
}

// safeFileName превращает произвольное имя в безопасное для ФС.
func safeFileName(name string) string {
	name = strings.TrimSpace(name)
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := strings.Trim(b.String(), "._")
	if out == "" {
		out = "file"
	}
	if len(out) > 120 {
		out = out[:120]
	}
	return out
}

// assistantText собирает видимый текст из записей assistant/message
// (reasoning-блоки отбрасываются).
func assistantEventText(ev sessionEvent) string {
	if ev.Type != "assistant/message" {
		return ""
	}
	var data struct {
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		return ""
	}
	var parts []string
	for _, c := range data.Message.Content {
		if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
			parts = append(parts, strings.TrimRight(c.Text, " \t\r\n"))
		}
	}
	return strings.Join(parts, "\n")
}

func assistantText(records []sessionRecord) string {
	// session/wait отдаёт записи в обратном порядке (новые первыми) — для
	// склейки ответа нужен хронологический порядок.
	recs := append([]sessionRecord(nil), records...)
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].Event.Seq < recs[j].Event.Seq })
	var parts []string
	for _, rec := range recs {
		if t := assistantEventText(rec.Event); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n\n")
}

// assistantTextEvents собирает видимый текст assistant/message после seq.
func assistantTextEvents(events []sessionEvent, since int64) string {
	var records []sessionRecord
	for _, ev := range events {
		if ev.Seq > since {
			records = append(records, sessionRecord{Type: "event", Event: ev})
		}
	}
	return assistantText(records)
}

// parseUserMessage разбирает user/message: kind (user/schedule/time-context),
// rpcId и текст.
func parseUserMessage(ev sessionEvent) (kind, rpcID, text string) {
	var d struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Source struct {
			Kind  string `json:"kind"`
			RPCID string `json:"rpcId"`
		} `json:"source"`
	}
	if json.Unmarshal(ev.Data, &d) != nil {
		return "", "", ""
	}
	var parts []string
	for _, c := range d.Content {
		if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
			parts = append(parts, strings.TrimRight(c.Text, " \t\r\n"))
		}
	}
	return d.Source.Kind, d.Source.RPCID, strings.Join(parts, "\n")
}

// pendingTurn — состояние незавершённого хода для watcher/worker.
type pendingTurn struct {
	Answer       string // последний видимый assistant-текст
	User         string // user-сообщения текущего/следующего хода
	External     bool   // текущий ход запущен не maxbot
	NextUser     string // user-сообщения, пришедшие во время активного хода
	NextExternal bool   // следующий ход запущен не maxbot
	Active       bool   // ход активен (между turn/start и turn/end)
	Progress     bool   // в активном ходе уже была работа (assistant/step/tool)
}

func joinUsers(users []string) string { return strings.Join(users, "\n\n") }

// turnResult — один завершённый ход: запрос оператора (если он внешний) и
// финальный ответ. Доставляются двумя отдельными сообщениями MAX.
type turnResult struct {
	User   string
	Text   string
	EndSeq int64
}

func joinTurnAnswers(trs []turnResult) string {
	var parts []string
	for _, t := range trs {
		if t.Text != "" {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// parseCompletedTurns разбирает окно событий от since: возвращает завершённые
// ходы (внешний запрос + финальный ответ) и seq последнего turn/end. Окно
// строится от last_answer_seq, поэтому user/message всегда попадает в него
// вместе с ответом; незавершённый ход не двигает курсор.
func parseCompletedTurns(events []sessionEvent, since int64, initUser string, initExternal bool, isOwn func(string) bool) ([]turnResult, int64, string, bool) {
	var turns []turnResult
	var turnUsers, nextUsers []string
	if initUser != "" {
		turnUsers = append(turnUsers, initUser)
	}
	turnExternal := initExternal
	nextExternal := false
	turnActive, progress := false, false
	current := ""
	var lastEnd int64
	carryUser := initUser
	carryExt := initExternal
	for _, ev := range events {
		if ev.Seq <= since {
			continue
		}
		switch ev.Type {
		case "user/message":
			kind, rpcID, text := parseUserMessage(ev)
			if kind != "user" || (rpcID != "" && isOwn(rpcID)) {
				continue
			}
			if turnActive && progress {
				if text != "" {
					nextUsers = append(nextUsers, text)
				}
				nextExternal = true
			} else {
				if text != "" {
					turnUsers = append(turnUsers, text)
				}
				turnExternal = true
			}
		case "turn/start":
			turnActive = true
			progress = false
		case "tool/call", "assistant/message":
			if turnActive {
				progress = true
			}
			if ev.Type == "assistant/message" {
				if t := assistantEventText(ev); t != "" {
					current = t
				}
			}
		case "turn/end":
			if current != "" {
				turns = append(turns, turnResult{User: joinUsers(turnUsers), Text: current, EndSeq: ev.Seq})
			}
			turnUsers = nextUsers
			nextUsers = nil
			turnExternal = nextExternal
			nextExternal = false
			turnActive = false
			progress = false
			current = ""
			lastEnd = ev.Seq
			// Переносим только запросы, промотированные на этом turn/end:
			// их события уже не попадут в следующее окно.
			carryUser = joinUsers(turnUsers)
			carryExt = turnExternal
		}
	}
	return turns, lastEnd, carryUser, carryExt
}

// turnUserFromEvents находит запрос, запустивший ход с endSeq, по событиям
// истории. Учитывает только внешние (не maxbot) user-сообщения и ту же
// атрибуцию turn/start/Progress, что и finalAssistantTextEvents.
func turnUserFromEvents(events []sessionEvent, endSeq int64, isOwn func(string) bool) (string, bool) {
	var turnUsers, nextUsers []string
	turnExternal, nextExternal := false, false
	turnActive, progress := false, false
	for _, ev := range events {
		if ev.Seq > endSeq {
			break
		}
		switch ev.Type {
		case "user/message":
			kind, rpcID, text := parseUserMessage(ev)
			if kind != "user" || (rpcID != "" && isOwn(rpcID)) {
				continue
			}
			if turnActive && progress {
				if text != "" {
					nextUsers = append(nextUsers, text)
				}
				nextExternal = true
			} else {
				if text != "" {
					turnUsers = append(turnUsers, text)
				}
				turnExternal = true
			}
		case "turn/start":
			turnActive = true
			progress = false
		case "tool/call", "assistant/message":
			if turnActive {
				progress = true
			}
		case "turn/end":
			if ev.Seq == endSeq {
				return joinUsers(turnUsers), turnExternal
			}
			turnUsers = nextUsers
			nextUsers = nil
			turnExternal = nextExternal
			nextExternal = false
			turnActive = false
			progress = false
		}
	}
	return "", false
}

// finalAssistantTextEvents разбирает видимый текст завершённых ходов после
// since. delivered — финальные ответы (последний assistant/message перед
// turn/end), в turnResult.User только для браузерных ходов
// (source.kind=user и rpcId не принадлежит maxbot). user/message во время
// активного хода буферизуются для следующего хода (учёт turn/start). isOwn
// сообщает, что rpcId отправлен maxbot.
func finalAssistantTextEvents(events []sessionEvent, since int64, p pendingTurn, isOwn func(string) bool) (delivered []turnResult, out pendingTurn, lastEnd int64) {
	var turnUsers []string
	if p.User != "" {
		turnUsers = append(turnUsers, p.User)
	}
	var nextUsers []string
	if p.NextUser != "" {
		nextUsers = append(nextUsers, p.NextUser)
	}
	turnExternal := p.External
	nextExternal := p.NextExternal
	turnActive := p.Active
	progress := p.Progress
	current := p.Answer

	for _, ev := range events {
		if ev.Seq <= since {
			continue
		}
		switch ev.Type {
		case "user/message":
			kind, rpcID, text := parseUserMessage(ev)
			if kind != "user" {
				continue // schedule/time-context — префикс не нужен
			}
			ownReq := rpcID != "" && isOwn(rpcID)
			if turnActive && progress {
				if !ownReq {
					if text != "" {
						nextUsers = append(nextUsers, text)
					}
					nextExternal = true
				}
			} else {
				if !ownReq {
					if text != "" {
						turnUsers = append(turnUsers, text)
					}
					turnExternal = true
				}
			}
		case "turn/start":
			turnActive = true
			progress = false
		case "tool/call":
			if turnActive {
				progress = true
			}
		case "assistant/message":
			if turnActive {
				progress = true
			}
			if t := assistantEventText(ev); t != "" {
				current = t
			}
		case "turn/end":
			if current != "" {
				res := turnResult{Text: current, EndSeq: ev.Seq}
				if turnExternal && len(turnUsers) > 0 {
					res.User = joinUsers(turnUsers)
				}
				delivered = append(delivered, res)
			}
			// Ход завершён: сообщения, пришедшие во время него, относятся к
			// следующему ходу.
			turnUsers = nextUsers
			nextUsers = nil
			turnExternal = nextExternal
			nextExternal = false
			turnActive = false
			progress = false
			current = ""
			lastEnd = ev.Seq
		}
	}
	out = pendingTurn{
		Answer:       current,
		User:         joinUsers(turnUsers),
		External:     turnExternal,
		NextUser:     joinUsers(nextUsers),
		NextExternal: nextExternal,
		Active:       turnActive,
		Progress:     progress,
	}
	return delivered, out, lastEnd
}

// turnInfo описывает интересные для замены сессии факты из новых событий.
type turnInfo struct {
	ContextOverflow bool
	MaxTokens       bool
	ToolCalls       []string
	Question        string // текст вопроса ask_user_question, если был
}

// analyzeEvents разбирает новые события: turn/end-причины и tool/call.
func analyzeEvents(events []sessionEvent) turnInfo {
	var ti turnInfo
	for _, ev := range events {
		switch ev.Type {
		case "turn/end":
			var d struct {
				Reason struct {
					Kind  string `json:"kind"`
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				} `json:"reason"`
			}
			if json.Unmarshal(ev.Data, &d) != nil {
				continue
			}
			switch d.Reason.Kind {
			case "max-tokens":
				ti.MaxTokens = true
			case "error":
				if d.Reason.Error.Code == "CONTEXT_WINDOW_EXCEEDED" {
					ti.ContextOverflow = true
				}
			}
		case "tool/call":
			var d struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if json.Unmarshal(ev.Data, &d) != nil {
				continue
			}
			args := string(d.Arguments)
			var asString string
			if json.Unmarshal(d.Arguments, &asString) == nil {
				args = asString
			}
			ti.ToolCalls = append(ti.ToolCalls, toolFingerprint(d.Name, args))
			if d.Name == "ask_user_question" {
				if q := formatAskQuestion(args); q != "" {
					ti.Question = q
				}
			}
		}
	}
	return ti
}

// formatAskQuestion форматирует аргументы ask_user_question для оператора.
func formatAskQuestion(arguments string) string {
	var d struct {
		Questions []struct {
			Question string `json:"question"`
			Options  []struct {
				Label string `json:"label"`
			} `json:"options"`
		} `json:"questions"`
	}
	if json.Unmarshal([]byte(arguments), &d) != nil || len(d.Questions) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Агент спрашивает:")
	for i, q := range d.Questions {
		fmt.Fprintf(&b, "\n%d) %s", i+1, strings.TrimSpace(q.Question))
		for _, o := range q.Options {
			if l := strings.TrimSpace(o.Label); l != "" {
				b.WriteString("\n   - " + l)
			}
		}
	}
	return b.String()
}

// sortedKeys — вспомогательная функция для стабильных сообщений (не для JSON).
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
