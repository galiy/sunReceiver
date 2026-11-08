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
	"strings"
	"testing"
	"time"
)

func TestDetectCycle(t *testing.T) {
	cases := []struct {
		name string
		seq  []string
		l    int
		ok   bool
	}{
		{"identical", []string{"a", "a", "a"}, 1, true},
		{"pair", []string{"a", "b", "a", "b", "a", "b"}, 2, true},
		{"triple", []string{"a", "b", "c", "a", "b", "c", "a", "b", "c"}, 3, true},
		{"two repeats only", []string{"a", "b", "a", "b"}, 0, false},
		{"no cycle", []string{"a", "b", "c", "d", "e", "f"}, 0, false},
		// Цикл сквозь «шум» не считается: блок должен повторяться подряд.
		{"noise between", []string{"a", "b", "x", "a", "b", "y", "a", "b"}, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, ok := detectCycle(c.seq)
			if ok != c.ok || l != c.l {
				t.Fatalf("detectCycle(%v) = (%d,%v), want (%d,%v)", c.seq, l, ok, c.l, c.ok)
			}
		})
	}
}

func TestSimilarity(t *testing.T) {
	if s := similarity("привет мир", "привет мир"); s != 1 {
		t.Fatalf("одинаковые строки: %v", s)
	}
	if s := similarity("", ""); s != 1 {
		t.Fatalf("пустые строки: %v", s)
	}
	if s := similarity("abcdef", "uvwxyz"); s >= 0.5 {
		t.Fatalf("разные строки слишком похожи: %v", s)
	}
	a := "ответ агента номер один, довольно длинный текст для проверки сходства"
	b := "ответ агента номер один, довольно длинный текст для проверки сходства!"
	if s := similarity(a, b); s < 0.9 {
		t.Fatalf("похожие строки: %v", s)
	}
}

func TestShouldReplaceAnswers(t *testing.T) {
	longA := strings.Repeat("одинаковый ответ агента ", 8)
	longB := strings.Repeat("одинаковый ответ агента ", 8) + "!"
	if shouldReplaceAnswers([]string{longA, longB}) {
		t.Fatal("двух ответов недостаточно для замены")
	}
	if !shouldReplaceAnswers([]string{longA, longB, longA}) {
		t.Fatal("три похожих ответа должны менять сессию")
	}
	if shouldReplaceAnswers([]string{longA, "совсем другой текст", longA}) {
		t.Fatal("разные ответы не должны менять сессию")
	}
}

func TestNextAnswerStreak(t *testing.T) {
	longA := strings.Repeat("длинный ответ ", 10)
	streak := nextAnswerStreak(nil, longA)
	if len(streak) != 1 {
		t.Fatalf("streak = %v", streak)
	}
	streak = nextAnswerStreak(streak, "ок")
	if len(streak) != 0 {
		t.Fatalf("короткий ответ должен рвать серию: %v", streak)
	}
	streak = nextAnswerStreak(nil, longA)
	for i := 0; i < 5; i++ {
		streak = nextAnswerStreak(streak, longA)
	}
	if len(streak) != 3 {
		t.Fatalf("streak должен хранить 3 ответа, got %d", len(streak))
	}
}

func TestModelExpired(t *testing.T) {
	now := time.Now()
	st := dshState{Provider: "wbllm", Model: "coder", ModelLastAPIAt: now.Add(-3 * time.Hour).UnixMilli()}
	if !modelExpired(st, now, 2*time.Hour, "wbllm", "coder-medium") {
		t.Fatal("3 часа простоя должны истекать окно")
	}
	st.ModelLastAPIAt = now.Add(-time.Hour).UnixMilli()
	if modelExpired(st, now, 2*time.Hour, "wbllm", "coder-medium") {
		t.Fatal("час простоя не истекает окно")
	}
	def := dshState{Provider: "wbllm", Model: "coder-medium", ModelLastAPIAt: now.Add(-5 * time.Hour).UnixMilli()}
	if modelExpired(def, now, 2*time.Hour, "wbllm", "coder-medium") {
		t.Fatal("модель по умолчанию не истекает")
	}
}

func TestToolFingerprintCanonical(t *testing.T) {
	a := toolFingerprint("bash", `{"b":2, "a":1}`)
	b := toolFingerprint("bash", `{"a":1,"b":2}`)
	if a != b {
		t.Fatalf("канонический JSON должен совпасть: %q != %q", a, b)
	}
	if toolFingerprint("bash", `{"a":1}`) == toolFingerprint("bash", `{"a":2}`) {
		t.Fatal("разные аргументы не должны совпадать")
	}
}

func TestLooksLikeMarkdown(t *testing.T) {
	if !looksLikeMarkdown("# Заголовок\nтекст") {
		t.Fatal("заголовок должен считаться markdown")
	}
	if !looksLikeMarkdown("```go\nfmt.Println()\n```") {
		t.Fatal("код-фенс должен считаться markdown")
	}
	if looksLikeMarkdown("просто текст без разметки") {
		t.Fatal("обычный текст не должен считаться markdown")
	}
}

func TestSafeFileName(t *testing.T) {
	if got := safeFileName("../../etc/passwd"); got != "passwd" {
		t.Fatalf("safeFileName = %q", got)
	}
	if got := safeFileName("отчёт 2026.pdf"); got == "" || strings.ContainsAny(got, "/\\ ") {
		t.Fatalf("safeFileName = %q", got)
	}
}

func TestAssistantTextOrdersBySeq(t *testing.T) {
	mk := func(seq int64, text string) sessionRecord {
		data := `{"message":{"content":[{"type":"text","text":"` + text + `"}]}}`
		return sessionRecord{Type: "event", Event: sessionEvent{Type: "assistant/message", Seq: seq, Data: json.RawMessage(data)}}
	}
	// wait отдаёт новые записи первыми.
	records := []sessionRecord{mk(3, "финал"), mk(1, "прогресс"), mk(2, "ещё прогресс")}
	if got := assistantText(records); got != "прогресс\n\nещё прогресс\n\nфинал" {
		t.Fatalf("assistantText = %q", got)
	}
}

func TestAssistantTextSkipsReasoning(t *testing.T) {
	data := `{"message":{"content":[
		{"type":"reasoning","text":"думаю"},
		{"type":"text","text":"ответ"}
	]}}`
	records := []sessionRecord{{
		Type:  "event",
		Event: sessionEvent{Type: "assistant/message", Data: json.RawMessage(data)},
	}}
	if got := assistantText(records); got != "ответ" {
		t.Fatalf("assistantText = %q", got)
	}
}

func TestTurnEcho(t *testing.T) {
	if got := turnEcho("вопрос"); got != "Вы писали:\nвопрос" {
		t.Fatalf("turnEcho = %q", got)
	}
	long := turnEcho(strings.Repeat("я", 5000))
	if n := len([]rune(long)); n != 4000 || !strings.HasSuffix(long, "…") {
		t.Fatalf("длинное эхо: %d символов, хвост %q", n, long[len(long)-5:])
	}
}

func TestDSHCommand(t *testing.T) {
	cases := map[string]string{
		"/menu":        "/menu",
		"/model":       "/model",
		"/cancel":      "/cancel",
		"/new":         "/new",
		"/continue":    "/continue",
		"/status":      "/status",
		"/model@mybot": "/model",
	}
	for in, want := range cases {
		got, ok := dshCommand(in)
		if !ok || got != want {
			t.Fatalf("dshCommand(%q) = (%q,%v), want (%q,true)", in, got, ok, want)
		}
	}
	for _, s := range []string{"/compact", "текст", "/unknown"} {
		if _, ok := dshCommand(s); ok {
			t.Fatalf("%q не должен перехватываться (это промпт)", s)
		}
	}
}

func TestAssistantTextEvents(t *testing.T) {
	data := `{"message":{"content":[{"type":"reasoning","text":"думаю"},{"type":"text","text":"итог"}]}}`
	events := []sessionEvent{
		{Type: "assistant/message", Seq: 3, Data: json.RawMessage(data)},
		{Type: "assistant/message", Seq: 8, Data: json.RawMessage(data)},
	}
	if got := assistantTextEvents(events, 4); got != "итог" {
		t.Fatalf("assistantTextEvents = %q, want только событие после seq=4", got)
	}
	if got := assistantTextEvents(events, 8); got != "" {
		t.Fatalf("после seq=8 не должно быть текста, got %q", got)
	}
}

func TestFinalAssistantTextEvents(t *testing.T) {
	msg := func(seq int64, text string) sessionEvent {
		data := `{"message":{"content":[{"type":"text","text":"` + text + `"}]}}`
		return sessionEvent{Type: "assistant/message", Seq: seq, Data: json.RawMessage(data)}
	}
	user := func(seq int64, kind, rpc, text string) sessionEvent {
		data := fmt.Sprintf(`{"content":[{"type":"text","text":%q}],"source":{"kind":%q,"rpcId":%q}}`, text, kind, rpc)
		return sessionEvent{Type: "user/message", Seq: seq, Data: json.RawMessage(data)}
	}
	end := func(seq int64) sessionEvent { return sessionEvent{Type: "turn/end", Seq: seq} }
	startEv := func(seq int64) sessionEvent { return sessionEvent{Type: "turn/start", Seq: seq} }
	notOwn := func(string) bool { return false }
	check := func(got []turnResult, want ...[2]string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("ходов %d, want %d: %+v", len(got), len(want), got)
		}
		for i := range want {
			if got[i].User != want[i][0] || got[i].Text != want[i][1] {
				t.Fatalf("ход %d = %+v, want user=%q text=%q", i, got[i], want[i][0], want[i][1])
			}
		}
	}

	events := []sessionEvent{
		msg(1, "промежуточно"), msg(2, "ответ 1"), end(3),
		msg(4, "снова проверяю"), msg(5, "ответ 2"), end(6),
	}
	got, out, lastEnd := finalAssistantTextEvents(events, 0, pendingTurn{}, notOwn)
	check(got, [2]string{"", "ответ 1"}, [2]string{"", "ответ 2"})
	if out.Answer != "" || lastEnd != 6 {
		t.Fatalf("без user: (%+v,%d)", out, lastEnd)
	}

	ext := []sessionEvent{
		user(1, "user", "browser-req", "Какие процессы?"),
		msg(2, "собираю"), msg(3, "готово"), end(4),
	}
	got, _, _ = finalAssistantTextEvents(ext, 0, pendingTurn{}, notOwn)
	check(got, [2]string{"Какие процессы?", "готово"})

	own := func(id string) bool { return id == "maxbot-req" }
	got, _, _ = finalAssistantTextEvents([]sessionEvent{
		user(1, "user", "maxbot-req", "мой запрос"), msg(2, "ответ"), end(3),
	}, 0, pendingTurn{}, own)
	check(got, [2]string{"", "ответ"})

	got, _, _ = finalAssistantTextEvents([]sessionEvent{
		user(1, "schedule", "", "[SCHEDULE REMINDER BATCH]"), msg(2, "ответ"), end(3),
	}, 0, pendingTurn{}, notOwn)
	check(got, [2]string{"", "ответ"})

	// user/message во время уже начавшегося хода относится к следующему ходу.
	got, _, _ = finalAssistantTextEvents([]sessionEvent{
		user(1, "user", "q1", "Вопрос A"), startEv(2),
		msg(3, "прогресс A"), user(4, "user", "q2", "Вопрос B"),
		msg(5, "ответ A"), end(6),
		msg(7, "ответ B"), end(8),
	}, 0, pendingTurn{}, notOwn)
	check(got, [2]string{"Вопрос A", "ответ A"}, [2]string{"Вопрос B", "ответ B"})

	// Ход, разбитый на два вызова (turn/start и user во время хода — в первом,
	// завершение — во втором).
	first := []sessionEvent{
		user(1, "user", "q1", "Вопрос A"), startEv(2),
		msg(3, "прогресс A"), user(4, "user", "q2", "Вопрос B"),
	}
	got, out, _ = finalAssistantTextEvents(first, 0, pendingTurn{}, notOwn)
	if len(got) != 0 || out.User != "Вопрос A" || out.NextUser != "Вопрос B" || !out.Active || !out.Progress {
		t.Fatalf("первый чанк: (%+v,%+v)", got, out)
	}
	second := []sessionEvent{msg(5, "ответ A"), end(6), msg(7, "ответ B"), end(8)}
	got, _, _ = finalAssistantTextEvents(second, 4, out, notOwn)
	check(got, [2]string{"Вопрос A", "ответ A"}, [2]string{"Вопрос B", "ответ B"})

	// Реальный порядок harness: turn/start ДО user/message, работа ещё не начата.
	got, _, _ = finalAssistantTextEvents([]sessionEvent{
		startEv(20), user(21, "user", "q3", "Вопрос C"),
		msg(22, "ответ C"), end(23),
	}, 0, pendingTurn{}, notOwn)
	check(got, [2]string{"Вопрос C", "ответ C"})

	// Незавершённый хвост не доставляется, но сохраняется.
	got, out, lastEnd = finalAssistantTextEvents([]sessionEvent{msg(10, "ещё проверяю")}, 0, pendingTurn{}, notOwn)
	if len(got) != 0 || out.Answer != "ещё проверяю" || lastEnd != 0 {
		t.Fatalf("незавершённый ход: (%+v,%+v,%d)", got, out, lastEnd)
	}
	// Pending-хвост пользователя склеивается с завершением.
	got, _, lastEnd = finalAssistantTextEvents(
		[]sessionEvent{end(11), msg(12, "ответ 2"), end(13)},
		0, pendingTurn{Answer: "ответ 1", User: "Вопрос", External: true}, notOwn)
	check(got, [2]string{"Вопрос", "ответ 1"}, [2]string{"", "ответ 2"})
	if lastEnd != 13 {
		t.Fatalf("lastEnd = %d", lastEnd)
	}
}

func TestAnalyzeEvents(t *testing.T) {
	mk := func(typ string, data string) sessionEvent {
		return sessionEvent{Type: typ, Data: json.RawMessage(data)}
	}
	events := []sessionEvent{
		mk("turn/end", `{"reason":{"kind":"error","error":{"code":"CONTEXT_WINDOW_EXCEEDED"}}}`),
		mk("tool/call", `{"name":"bash","arguments":"{\"cmd\":\"ls\"}"}`),
	}
	ti := analyzeEvents(events)
	if !ti.ContextOverflow {
		t.Fatal("ожидали CONTEXT_WINDOW_EXCEEDED")
	}
	if len(ti.ToolCalls) != 1 {
		t.Fatalf("tool calls = %v", ti.ToolCalls)
	}

	if !analyzeEvents([]sessionEvent{mk("turn/end", `{"reason":{"kind":"max-tokens"}}`)}).MaxTokens {
		t.Fatal("ожидали max-tokens")
	}
}

func TestFormatAskQuestion(t *testing.T) {
	args := `{"questions":[{"question":"Что делать?","options":[{"label":"Вариант A"},{"label":"Вариант B"}]}]}`
	got := formatAskQuestion(args)
	if !strings.Contains(got, "Что делать?") || !strings.Contains(got, "Вариант A") {
		t.Fatalf("formatAskQuestion = %q", got)
	}
	if formatAskQuestion("not json") != "" {
		t.Fatal("битый JSON должен давать пустую строку")
	}
}
