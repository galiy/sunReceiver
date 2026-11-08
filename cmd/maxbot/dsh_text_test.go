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
	end := func(seq int64) sessionEvent { return sessionEvent{Type: "turn/end", Seq: seq} }
	events := []sessionEvent{
		msg(1, "промежуточно"), msg(2, "ответ 1"), end(3),
		msg(4, "снова проверяю"), msg(5, "ответ 2"), end(6),
	}
	if got, tail, lastEnd := finalAssistantTextEvents(events, 0, ""); got != "ответ 1\n\nответ 2" || tail != "" || lastEnd != 6 {
		t.Fatalf("finalAssistantTextEvents = (%q,%q,%d)", got, tail, lastEnd)
	}
	if got, tail, lastEnd := finalAssistantTextEvents(events, 5, ""); got != "" || tail != "" || lastEnd != 6 {
		t.Fatalf("после seq=5: (%q,%q,%d)", got, tail, lastEnd)
	}
	// Незавершённый хвост не доставляется, но сохраняется как tail.
	events2 := []sessionEvent{msg(10, "ещё проверяю")}
	if got, tail, lastEnd := finalAssistantTextEvents(events2, 0, ""); got != "" || tail != "ещё проверяю" || lastEnd != 0 {
		t.Fatalf("незавершённый ход: (%q,%q,%d)", got, tail, lastEnd)
	}
	// Сохранённый хвост склеивается с завершением, даже если в том же окне
	// есть ещё один завершённый ход.
	events3 := []sessionEvent{end(11), msg(12, "ответ 2"), end(13)}
	if got, tail, lastEnd := finalAssistantTextEvents(events3, 0, "ответ 1"); got != "ответ 1\n\nответ 2" || tail != "" || lastEnd != 13 {
		t.Fatalf("pending + два хода: (%q,%q,%d)", got, tail, lastEnd)
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
