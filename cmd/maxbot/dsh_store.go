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
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	dshStateKey      = "sunreceiver:maxbot:dsh:state"
	dshQueueKey      = "sunreceiver:maxbot:dsh:queue"
	dshProcessingKey = "sunreceiver:maxbot:dsh:processing"
	maxMarkerKey     = "sunreceiver:maxbot:max_marker"
)

// dshState — сохраняемое в Redis состояние диалога с harness.
type dshState struct {
	SessionID        string
	Provider         string
	Model            string
	Effort           string
	ModelSelectedAt  int64 // unix ms
	ModelLastAPIAt   int64 // unix ms, обновляется на каждом запросе
	LastEventSeq     int64
	LastAnswerSeq    int64
	LastError        string
	ToolSeq          []string
	RecentAnswers    []string
	AwaitingInput    bool
	InputSeq         int64  // курсор обработанного input-required (дедуп вопросов)
	PendingSchedText string // хвост assistant-текста без turn/end
}

// dshQueueItem — элемент очереди промптов.
type dshQueueItem struct {
	Kind        string          `json:"kind"` // prompt | continue
	RequestID   string          `json:"request_id"`
	Text        string          `json:"text"`
	Attachments []maxAttachment `json:"attachments,omitempty"`
	Retried     bool            `json:"retried,omitempty"`
	Epoch       int64           `json:"epoch,omitempty"` // эпоха отмены на момент постановки
	TS          int64           `json:"ts"`
}

func loadDSHState(ctx context.Context, rdb *redis.Client) (dshState, error) {
	var st dshState
	m, err := rdb.HGetAll(ctx, dshStateKey).Result()
	if err != nil {
		return st, err
	}
	st.SessionID = m["session_id"]
	st.Provider = m["provider"]
	st.Model = m["model"]
	st.Effort = m["effort"]
	st.ModelSelectedAt = parseInt64(m["model_selected_at"])
	st.ModelLastAPIAt = parseInt64(m["model_last_api_at"])
	st.LastEventSeq = parseInt64(m["last_event_seq"])
	st.LastAnswerSeq = parseInt64(m["last_answer_seq"])
	st.LastError = m["last_error"]
	st.AwaitingInput = m["awaiting_input"] == "1"
	st.InputSeq = parseInt64(m["input_seq"])
	st.PendingSchedText = m["pending_sched_text"]
	if v := m["tool_seq"]; v != "" {
		_ = json.Unmarshal([]byte(v), &st.ToolSeq)
	}
	if v := m["recent_answers"]; v != "" {
		_ = json.Unmarshal([]byte(v), &st.RecentAnswers)
	}
	return st, nil
}

func saveDSHState(ctx context.Context, rdb *redis.Client, st *dshState) error {
	toolSeq, _ := json.Marshal(st.ToolSeq)
	answers, _ := json.Marshal(st.RecentAnswers)
	awaiting := "0"
	if st.AwaitingInput {
		awaiting = "1"
	}
	return rdb.HSet(ctx, dshStateKey, map[string]any{
		"session_id":         st.SessionID,
		"provider":           st.Provider,
		"model":              st.Model,
		"effort":             st.Effort,
		"model_selected_at":  strconv.FormatInt(st.ModelSelectedAt, 10),
		"model_last_api_at":  strconv.FormatInt(st.ModelLastAPIAt, 10),
		"last_event_seq":     strconv.FormatInt(st.LastEventSeq, 10),
		"last_answer_seq":    strconv.FormatInt(st.LastAnswerSeq, 10),
		"last_error":         st.LastError,
		"tool_seq":           string(toolSeq),
		"recent_answers":     string(answers),
		"awaiting_input":     awaiting,
		"input_seq":          strconv.FormatInt(st.InputSeq, 10),
		"pending_sched_text": st.PendingSchedText,
	}).Err()
}

func parseInt64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// pushDSHQueue добавляет элемент в очередь (FIFO: LPUSH + BRPOPLPUSH).
func pushDSHQueue(ctx context.Context, rdb *redis.Client, item dshQueueItem) error {
	b, err := json.Marshal(item)
	if err != nil {
		return err
	}
	return rdb.LPush(ctx, dshQueueKey, string(b)).Err()
}

// popDSHQueue забирает элемент из очереди в processing.
func popDSHQueue(ctx context.Context, rdb *redis.Client, block time.Duration) (string, error) {
	return rdb.BRPopLPush(ctx, dshQueueKey, dshProcessingKey, block).Result()
}

// removeDSHProcessing удаляет обработанный элемент из processing.
func removeDSHProcessing(ctx context.Context, rdb *redis.Client, raw string) {
	rdb.LRem(ctx, dshProcessingKey, 1, raw)
}

// recoverScript атомарно переносит зависшие элементы processing в очередь так,
// чтобы самым старым (первым к выполнению) оказался самый старый из processing.
// processing: голова — самый новый; RPOP даёт oldest→newest, затем пушим в
// обратном порядке (newest→oldest) в хвост очереди.
var recoverScript = redis.NewScript(`
local items = {}
while true do
  local v = redis.call('RPOP', KEYS[1])
  if not v then break end
  table.insert(items, v)
end
for i = #items, 1, -1 do
  redis.call('RPUSH', KEYS[2], items[i])
end
return #items`)

// recoverDSHProcessing возвращает элементы, зависшие в processing после
// рестарта, в очередь.
func recoverDSHProcessing(ctx context.Context, rdb *redis.Client) {
	if _, err := recoverScript.Run(ctx, rdb, []string{dshProcessingKey, dshQueueKey}).Int(); err != nil {
		log.Printf("maxbot: восстановление dsh-processing: %v", err)
	}
}

// clearDSHQueue очищает очередь и processing (отмена/новая сессия).
func clearDSHQueue(ctx context.Context, rdb *redis.Client) error {
	return rdb.Del(ctx, dshQueueKey, dshProcessingKey).Err()
}

// dshQueueLen возвращает длину очереди (без processing).
func dshQueueLen(ctx context.Context, rdb *redis.Client) int64 {
	n, err := rdb.LLen(ctx, dshQueueKey).Result()
	if err != nil {
		return 0
	}
	return n
}

// saveMaxMarker сохраняет marker long polling, чтобы не переобрабатывать
// сообщения после рестарта.
func saveMaxMarker(ctx context.Context, rdb *redis.Client, marker int64) {
	if rdb == nil || marker <= 0 {
		return
	}
	rdb.Set(ctx, maxMarkerKey, strconv.FormatInt(marker, 10), 0)
}

func loadMaxMarker(ctx context.Context, rdb *redis.Client) int64 {
	if rdb == nil {
		return 0
	}
	v, err := rdb.Get(ctx, maxMarkerKey).Result()
	if err != nil {
		return 0
	}
	return parseInt64(v)
}

func fmtAck(n int64) string {
	return fmt.Sprintf("в очереди: %d", n)
}
