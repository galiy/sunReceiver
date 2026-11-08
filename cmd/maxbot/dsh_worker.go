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
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

var (
	errDSHCancelled     = errors.New("dsh: отменено оператором")
	errModelUnavailable = errors.New("dsh: модель недоступна")
)

// dshWaitResult — итог ожидания хода агента.
type dshWaitResult struct {
	Text          string // видимый ответ (может быть частичным при замене)
	Question      string // вопрос агента (input-required)
	ReplaceReason string // непусто — нужно заменить сессию
}

// dshEnabled — включён ли прокси-чат с harness.
func (b *bot) dshEnabled() bool {
	return b != nil && b.dsh != nil && b.dshCfg != nil && !b.dshCfg.Disabled
}

// ---- состояние ----

func copyDSHState(st dshState) dshState {
	st.ToolSeq = append([]string(nil), st.ToolSeq...)
	st.RecentAnswers = append([]string(nil), st.RecentAnswers...)
	return st
}

func (b *bot) dshSnapshot() dshState {
	b.dshMu.Lock()
	defer b.dshMu.Unlock()
	return copyDSHState(b.dshState)
}

// dshSet меняет состояние под dshMu, а сохраняет его в Redis без удержания
// dshMu. Версия состояния не даёт более старому снимку перезаписать новый.
func (b *bot) dshSet(ctx context.Context, fn func(*dshState)) {
	b.dshMu.Lock()
	fn(&b.dshState)
	b.dshStateVer++
	ver := b.dshStateVer
	st := copyDSHState(b.dshState)
	b.dshMu.Unlock()

	if b.rdb == nil {
		return
	}
	b.dshSaveMu.Lock()
	defer b.dshSaveMu.Unlock()
	b.dshMu.Lock()
	latest := b.dshStateVer
	b.dshMu.Unlock()
	if latest != ver {
		return // более новое состояние сохранит (или уже сохранил) свой вызов
	}
	if err := saveDSHState(ctx, b.rdb, &st); err != nil {
		log.Printf("maxbot: сохранение dsh-состояния: %v", err)
	}
}

// touchAPI отмечает запрос к API (продлевает окно выбранной модели).
func (b *bot) touchAPI(ctx context.Context) {
	b.dshSet(ctx, func(st *dshState) {
		st.ModelLastAPIAt = time.Now().UnixMilli()
	})
}

// retryDSH повторяет транспортные ошибки; бизнес-ошибки API не повторяются.
func (b *bot) retryDSH(ctx context.Context, fn func() error) error {
	attempts := 3
	if b.dshCfg != nil && b.dshCfg.RetryAttempts > 0 {
		attempts = b.dshCfg.RetryAttempts
	}
	var err error
	for i := 0; i < attempts; i++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err = fn()
		if err == nil {
			return nil
		}
		var apiErr *dshAPIError
		if errors.As(err, &apiErr) {
			return err
		}
		if i+1 < attempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(1<<i) * time.Second):
			}
		}
	}
	return err
}

// ---- очередь ----

// runDSH — основной цикл чата с harness.
func (b *bot) runDSH(ctx context.Context) {
	if !b.dshEnabled() {
		return
	}
	st, err := loadDSHState(ctx, b.rdb)
	if err != nil {
		log.Printf("maxbot: загрузка dsh-состояния: %v", err)
	}
	b.dshMu.Lock()
	b.dshState = st
	if b.dshState.Provider == "" {
		b.dshState.Provider = b.dshCfg.Provider
	}
	if b.dshState.Model == "" {
		b.dshState.Model = b.dshCfg.DefaultModel
	}
	if b.dshState.Effort == "" {
		b.dshState.Effort = b.dshCfg.Effort
	}
	// После рестарта зависший вопрос мог остаться: даём watcher'у обработать его
	// (переслать/остановить) один раз.
	b.dshState.AwaitingInput = false
	b.dshState.InputSeq = 0
	b.dshMu.Unlock()
	recoverDSHProcessing(ctx, b.rdb)
	go b.runDSHModelTTL(ctx)
	log.Printf("maxbot: dsh-чат запущен (harness %s, workspace %s)", b.dshCfg.BaseURL, b.dshCfg.Workspace)

	for {
		if ctx.Err() != nil {
			return
		}
		raw, err := popDSHQueue(ctx, b.rdb, 5*time.Second)
		if ctx.Err() != nil {
			return
		}
		if err == redis.Nil {
			continue
		}
		if err != nil {
			log.Printf("maxbot: чтение dsh-очереди: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		var item dshQueueItem
		if err := json.Unmarshal([]byte(raw), &item); err != nil {
			log.Printf("maxbot: битый элемент dsh-очереди: %v", err)
			removeDSHProcessing(ctx, b.rdb, raw)
			continue
		}
		err = b.processDSHItem(ctx, item)
		if ctx.Err() != nil {
			// Остановка сервиса: элемент остаётся в processing и будет
			// возвращён в очередь при следующем старте.
			return
		}
		if err != nil {
			log.Printf("maxbot: обработка dsh-запроса: %v", err)
		}
		removeDSHProcessing(ctx, b.rdb, raw)
	}
}

// registerBotCommands регистрирует команды меню MAX (лучшая попытка).
func (b *bot) registerBotCommands(ctx context.Context) {
	cmds := []maxBotCommand{
		{Name: "menu", Description: "Меню бота (кнопки)"},
		{Name: "model", Description: "Выбрать модель агента"},
		{Name: "cancel", Description: "Отменить текущую задачу и очистить очередь"},
		{Name: "continue", Description: "Продолжить с места остановки"},
		{Name: "new", Description: "Новая сессия агента"},
		{Name: "status", Description: "Текущая модель, сессия и очередь"},
	}
	if err := b.client.setCommands(ctx, cmds); err != nil {
		log.Printf("maxbot: регистрация команд MAX: %v", err)
	}
}

// requeueDSHItem ставит элемент в начало очереди (следующим к выполнению).
// Эпоха элемента сохраняется прежней; если отмена уже произошла, не ставим.
func (b *bot) requeueDSHItem(ctx context.Context, item dshQueueItem, epoch int64) error {
	if b.dshCancelEpoch.Load() != epoch {
		return nil
	}
	item.Retried = true
	item.Epoch = epoch
	raw, err := json.Marshal(item)
	if err != nil {
		return err
	}
	return b.rdb.RPush(ctx, dshQueueKey, string(raw)).Err()
}

// processDSHItem выполняет один запрос оператора. Эпоха отмены берётся из
// самого элемента (момент постановки в очередь): если она уже сменилась, запрос
// не отправляем (его успели отменить). Новый запрос, поставленный после
// отмены, несёт новую эпоху и не теряется.
func (b *bot) processDSHItem(ctx context.Context, item dshQueueItem) error {
	epoch := item.Epoch
	if epoch == 0 {
		epoch = b.dshCancelEpoch.Load()
	}
	if b.dshCancelEpoch.Load() != epoch {
		return nil
	}
	// Весь ход (промпт + ожидание + доставка) под pumpMu: фоновый watcher не
	// должен вмешиваться и, например, заменить сессию посреди хода.
	b.dshPumpMu.Lock()
	defer b.dshPumpMu.Unlock()
	b.dshBusy.Store(true)
	defer b.dshBusy.Store(false)

	if item.Kind == "continue" && strings.TrimSpace(item.Text) == "" {
		item.Text = "продолжи с места остановки"
	}
	// Вложения скачиваются уже в воркере, чтобы long polling MAX не блокировался.
	images, files := b.collectAttachments(ctx, item.Attachments)
	text := item.Text
	if len(files) > 0 {
		var sb strings.Builder
		sb.WriteString(text)
		for _, p := range files {
			if sb.Len() > 0 {
				sb.WriteString("\n\n")
			}
			sb.WriteString("[Файл: " + p + "]")
		}
		text = sb.String()
	}
	var content []promptPart
	if strings.TrimSpace(text) != "" {
		content = append(content, promptPart{Type: "text", Text: text})
	}
	content = append(content, images...)
	if len(content) == 0 {
		return nil
	}

	sessionID, err := b.ensureDSHSession(ctx)
	if err != nil {
		if errors.Is(err, errModelUnavailable) {
			return nil // оператор уже уведомлён, ждём выбора модели
		}
		b.notifyOperator(ctx, "Не удалось открыть сессию агента: "+err.Error())
		return err
	}
	b.dshSet(ctx, func(st *dshState) { st.AwaitingInput = false; st.InputSeq = 0; st.PendingSchedText = "" })
	if b.dshCancelEpoch.Load() != epoch {
		return nil
	}

	if err := b.retryDSH(ctx, func() error {
		return b.dsh.prompt(ctx, sessionID, item.RequestID, content)
	}); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, errDSHCancelled) {
			return nil
		}
		b.dshSet(ctx, func(st *dshState) { st.LastError = err.Error() })
		b.notifyOperator(ctx, "Не удалось отправить запрос агенту: "+err.Error())
		return err
	}
	b.touchAPI(ctx)
	if b.dshCancelEpoch.Load() != epoch {
		// Пока запрос летел, оператор отменил/сменил сессию — гасим ход.
		_ = b.dsh.cancel(ctx, sessionID)
		return nil
	}

	res, err := b.waitForAnswer(ctx, sessionID, epoch)
	if err != nil {
		if errors.Is(err, errDSHCancelled) || errors.Is(err, context.Canceled) {
			return nil
		}
		b.dshSet(ctx, func(st *dshState) { st.LastError = err.Error() })
		b.notifyOperator(ctx, "Ожидание ответа агента прервано: "+err.Error())
		return err
	}
	if b.dshCancelEpoch.Load() != epoch {
		return nil
	}
	if res.Text != "" {
		if err := b.sendAnswerText(ctx, res.Text); err != nil {
			log.Printf("maxbot: отправка ответа агента: %v", err)
		}
		b.touchAPI(ctx)
	}
	// Отмена во время доставки ответа гасит пост-обработку (вопрос/замену).
	if b.dshCancelEpoch.Load() != epoch {
		return nil
	}
	if res.Question != "" {
		b.notifyOperator(ctx, res.Question)
		b.dshSet(ctx, func(st *dshState) { st.AwaitingInput = true })
		return nil
	}
	if res.ReplaceReason != "" {
		if err := b.replaceDSHSessionLocked(ctx, res.ReplaceReason, epoch); err != nil {
			b.notifyOperator(ctx, "Не удалось создать новую сессию: "+err.Error())
			return err
		}
		if b.dshCancelEpoch.Load() != epoch {
			return nil // отменили/сменили сессию во время восстановления
		}
		if !item.Retried {
			if err := b.requeueDSHItem(ctx, item, epoch); err != nil {
				log.Printf("maxbot: возврат запроса в очередь: %v", err)
			}
		} else {
			b.notifyOperator(ctx, "Запрос не выполнен после замены сессии. Переформулируйте или используйте /continue.")
		}
		return nil
	}
	b.recordAnswer(ctx, res.Text, epoch)
	return nil
}

// ---- сессия и модель ----

// ensureDSHSession возвращает текущую сессию, создавая её при необходимости.
func (b *bot) ensureDSHSession(ctx context.Context) (string, error) {
	b.dshSessionMu.Lock()
	defer b.dshSessionMu.Unlock()
	st := b.dshSnapshot()
	if st.SessionID != "" {
		return st.SessionID, nil
	}
	provider, model, effort := st.Provider, st.Model, st.Effort
	if provider == "" {
		provider = b.dshCfg.Provider
	}
	if model == "" {
		model = b.dshCfg.DefaultModel
	}
	if effort == "" {
		effort = b.dshCfg.Effort
	}
	return b.createDSHSessionLocked(ctx, provider, model, effort)
}

// createDSHSession создаёт новую сессию (сериализовано с другими сменами).
func (b *bot) createDSHSession(ctx context.Context, provider, model, effort string) (string, error) {
	b.dshSessionMu.Lock()
	defer b.dshSessionMu.Unlock()
	return b.createDSHSessionLocked(ctx, provider, model, effort)
}

// createDSHSessionLocked создаёт новую сессию, проверяет модель и применяет её.
// Вызывающий держит dshSessionMu.
func (b *bot) createDSHSessionLocked(ctx context.Context, provider, model, effort string) (string, error) {
	cat, err := b.dsh.modelCatalog(ctx)
	if err != nil {
		return "", err
	}
	b.touchAPI(ctx)
	if provider == "" {
		provider = b.dshCfg.Provider
	}
	if model == "" {
		model = b.dshCfg.DefaultModel
	}
	if effort == "" {
		effort = b.dshCfg.Effort
	}
	if !cat.hasModel(provider, model) {
		b.notifyOperator(ctx, fmt.Sprintf("Модель %s/%s недоступна. Выберите модель командой /model.", provider, model))
		return "", errModelUnavailable
	}
	applyEffort := ""
	if cat.supportsEffort(provider, model, effort) {
		applyEffort = effort
	}

	sessionID, err := b.dsh.createSession(ctx, b.dshCfg.Workspace)
	if err != nil {
		return "", err
	}
	b.touchAPI(ctx)
	if err := b.dsh.selectModel(ctx, sessionID, provider, model, applyEffort); err != nil {
		_ = b.dsh.cancel(ctx, sessionID)
		return "", err
	}
	b.touchAPI(ctx)

	now := time.Now().UnixMilli()
	b.dshSet(ctx, func(st *dshState) {
		st.SessionID = sessionID
		st.Provider = provider
		st.Model = model
		st.Effort = applyEffort
		if st.Model == b.dshCfg.DefaultModel && st.Provider == b.dshCfg.Provider {
			st.ModelSelectedAt = 0
		} else {
			st.ModelSelectedAt = now
		}
		st.ModelLastAPIAt = now
		st.LastEventSeq = 0
		st.LastAnswerSeq = 0
		st.ToolSeq = nil
		st.RecentAnswers = nil
		st.AwaitingInput = false
		st.InputSeq = 0
		st.PendingSchedText = ""
	})
	log.Printf("maxbot: dsh-сессия %s, модель %s/%s (effort %q)", sessionID, provider, model, applyEffort)
	return sessionID, nil
}

// replaceDSHSession создаёт новую сессию и просит агента восстановить контекст.
// epoch — эпоха отмены: если она сменилась (оператор нажал /cancel или /new),
// замена прерывается, а уже созданная сессия отменяется.
func (b *bot) replaceDSHSession(ctx context.Context, reason string, epoch int64, expectedSession string) error {
	b.dshPumpMu.Lock()
	defer b.dshPumpMu.Unlock()
	// Пока ждали pump, воркер мог сам заменить сессию — не заменяем новую повторно.
	if expectedSession != "" && b.dshSnapshot().SessionID != expectedSession {
		return nil
	}
	return b.replaceDSHSessionLocked(ctx, reason, epoch)
}

// replaceDSHSessionLocked — то же, но вызывающий уже держит dshPumpMu.
func (b *bot) replaceDSHSessionLocked(ctx context.Context, reason string, epoch int64) error {
	if b.dshCancelEpoch.Load() != epoch {
		return nil
	}
	st := b.dshSnapshot()
	old := st.SessionID
	provider, model, effort := st.Provider, st.Model, st.Effort
	if provider == "" {
		provider = b.dshCfg.Provider
	}
	if model == "" {
		model = b.dshCfg.DefaultModel
	}
	if effort == "" {
		effort = b.dshCfg.Effort
	}
	// Останавливаем старую сессию, чтобы она не продолжала работу в фоне.
	if old != "" {
		if err := b.dsh.cancel(ctx, old); err != nil {
			log.Printf("maxbot: отмена старой dsh-сессии %s: %v", old, err)
		}
	}
	newID, err := b.createDSHSession(ctx, provider, model, effort)
	if err != nil {
		return err
	}
	if b.dshCancelEpoch.Load() != epoch {
		_ = b.dsh.cancel(ctx, newID)
		return nil
	}
	b.notifyOperator(ctx, "⚠️ Сессия агента заменена: "+reason)
	b.notifyOperator(ctx, "Восстанавливаю контекст предыдущей сессии…")
	recovery := fmt.Sprintf(
		"Предыдущая сессия %s завершилась (%s). Найди её историю (рабочая область %s) и восстанови хвост и ключевые последние факты. Не пересказывай процесс — просто восстанови контекст и будь готов продолжить работу.", old, reason, b.dshCfg.Workspace)
	if b.dshCancelEpoch.Load() != epoch {
		_ = b.dsh.cancel(ctx, newID)
		return nil
	}
	if err := b.retryDSH(ctx, func() error {
		return b.dsh.prompt(ctx, newID, newUUID(), []promptPart{{Type: "text", Text: recovery}})
	}); err != nil {
		log.Printf("maxbot: запрос восстановления контекста: %v", err)
		return nil
	}
	b.touchAPI(ctx)
	b.waitDiscard(ctx, newID, epoch)
	return nil
}

// waitDiscard ждёт завершения восстановительного хода, не отправляя его текст.
func (b *bot) waitDiscard(ctx context.Context, sessionID string, epoch int64) {
	deadline := time.Now().Add(15 * time.Minute)
	lastBeat := time.Now()
	for {
		if ctx.Err() != nil || time.Now().After(deadline) || b.dshCancelEpoch.Load() != epoch {
			return
		}
		st := b.dshSnapshot()
		wv, err := b.dsh.wait(ctx, sessionID, st.LastAnswerSeq, 30000)
		if err != nil {
			log.Printf("maxbot: ожидание восстановления контекста: %v", err)
			return
		}
		b.touchAPI(ctx)
		if wv.Reason == "idle" || wv.Reason == "input-required" {
			b.dshSet(ctx, func(s *dshState) {
				s.LastAnswerSeq = wv.State.Cursor
				s.LastEventSeq = wv.State.Cursor
			})
			return
		}
		if time.Since(lastBeat) >= time.Duration(b.dshCfg.HeartbeatSec)*time.Second {
			b.notifyOperator(ctx, "Агент работает")
			lastBeat = time.Now()
		}
	}
}

// ---- ожидание ответа ----

// waitForAnswer крутит session/wait, шлёт heartbeat и анализирует события.
// waitForAnswer вызывается только из processDSHItem, который держит dshPumpMu.
func (b *bot) waitForAnswer(ctx context.Context, sessionID string, epoch int64) (dshWaitResult, error) {
	var res dshWaitResult
	lastBeat := time.Now()
	for {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		if b.dshCancelEpoch.Load() != epoch {
			return res, errDSHCancelled
		}
		st := b.dshSnapshot()
		wv, err := b.dsh.wait(ctx, sessionID, st.LastAnswerSeq, 30000)
		if err != nil {
			return res, err
		}
		if b.dshCancelEpoch.Load() != epoch {
			return res, errDSHCancelled
		}
		b.touchAPI(ctx)

		events, _ := b.fetchNewEvents(ctx, sessionID, wv.State.Cursor)
		ti := analyzeEvents(events)
		cycle, cycleLen := b.addToolFingerprints(ctx, ti.ToolCalls)
		if ti.ContextOverflow || ti.MaxTokens || cycle {
			res.Text = assistantText(wv.Output.Records)
			b.dshSet(ctx, func(s *dshState) { s.LastAnswerSeq = wv.State.Cursor })
			switch {
			case ti.ContextOverflow:
				res.ReplaceReason = "переполнение контекста"
			case ti.MaxTokens:
				res.ReplaceReason = "достигнут лимит вывода (max-tokens)"
			default:
				res.ReplaceReason = fmt.Sprintf("цикл tool/call (период %d, 3 повтора)", cycleLen)
			}
			return res, nil
		}
		if ti.Question != "" && (wv.Reason == "input-required" || hasPendingQuestion(wv.State.PendingInput)) {
			res.Question = b.handleInputRequired(ctx, sessionID, ti.Question, wv.State.Cursor, epoch)
			return res, nil
		}
		switch wv.Reason {
		case "idle":
			res.Text = assistantText(wv.Output.Records)
			b.dshSet(ctx, func(s *dshState) { s.LastAnswerSeq = wv.State.Cursor })
			return res, nil
		case "input-required":
			res.Question = b.handleInputRequired(ctx, sessionID, "", wv.State.Cursor, epoch)
			return res, nil
		}

		if b.dshCfg.HeartbeatSec > 0 && time.Since(lastBeat) >= time.Duration(b.dshCfg.HeartbeatSec)*time.Second {
			b.notifyOperator(ctx, "Агент работает")
			lastBeat = time.Now()
		}
	}
}

// handleInputRequired пересылает вопрос оператору и останавливает зависший ход.
// harness не таймаутит ask_user_question, а ответить на живой вопрос через
// unary-API нельзя; поэтому ход отменяется, а следующее сообщение оператора —
// обычный новый промпт. Повторные уведомления по тому же ожиданию подавляются
// флагом AwaitingInput (сбрасывается при следующем промпте).
func (b *bot) handleInputRequired(ctx context.Context, sessionID, question string, cursor, epoch int64) string {
	if b.dshCancelEpoch.Load() != epoch {
		return ""
	}
	st := b.dshSnapshot()
	if st.AwaitingInput && st.InputSeq == cursor {
		// Тот же самый вопрос уже обработан — повторно не шлём и не отменяем.
		b.dshSet(ctx, func(s *dshState) { s.LastAnswerSeq = cursor })
		return ""
	}
	if question == "" {
		question = "Агент ждёт ответа."
	}
	cancelErr := b.dsh.cancel(ctx, sessionID)
	if b.dshCancelEpoch.Load() != epoch {
		return ""
	}
	note := "Ход остановлен: ответ на вопрос агента не поддерживается. Отправьте ответ новым сообщением."
	if cancelErr != nil {
		note = "Не удалось остановить ход агента: " + cancelErr.Error() + ". Попробуйте /cancel."
	}
	b.dshSet(ctx, func(s *dshState) {
		s.AwaitingInput = true
		s.InputSeq = cursor
		s.LastAnswerSeq = cursor
		// Хвост отменённого хода не должен «прилипнуть» к следующему результату.
		s.PendingSchedText = ""
	})
	return question + "\n\n" + note
}

// unblockAwaitingInput гасит зависший вопрос перед новым промптом оператора:
// иначе harness оставит новый промпт в очереди за неотвеченным вопросом, а мы
// снова получим input-required и отменим уже новый ход.
func (b *bot) unblockAwaitingInput(ctx context.Context) error {
	st := b.dshSnapshot()
	if !st.AwaitingInput {
		return nil
	}
	if st.SessionID != "" {
		if err := b.dsh.cancel(ctx, st.SessionID); err != nil {
			return err
		}
		b.touchAPI(ctx)
	}
	b.dshSet(ctx, func(s *dshState) {
		s.AwaitingInput = false
		s.InputSeq = 0
		s.PendingSchedText = ""
	})
	return nil
}

// hasPendingQuestion — есть ли среди ожидающих входов вопрос агента.
func hasPendingQuestion(inputs []string) bool {
	for _, in := range inputs {
		if in == "question" {
			return true
		}
	}
	return false
}

// fetchNewEvents добирает события после LastEventSeq (страницы истории).
// complete=false означает ошибку чтения или неполную выборку (лимит страниц):
// в этом случае вызывающий не должен сдвигать курсоры доставки/событий.
func (b *bot) fetchNewEvents(ctx context.Context, sessionID string, cursor int64) ([]sessionEvent, bool) {
	st := b.dshSnapshot()
	since := st.LastEventSeq
	if cursor <= since {
		return nil, true
	}
	through := cursor
	seen := make(map[int64]sessionEvent)
	complete := false
	for page := 0; page < 5; page++ {
		p, err := b.dsh.events(ctx, sessionID, through, 300)
		if err != nil {
			log.Printf("maxbot: чтение событий dsh: %v", err)
			break
		}
		var minSeq int64
		for _, r := range p.Records {
			if r.Event.Seq > since {
				seen[r.Event.Seq] = r.Event
			}
			if minSeq == 0 || r.Event.Seq < minSeq {
				minSeq = r.Event.Seq
			}
		}
		if !p.HasMore {
			complete = true
			break
		}
		if minSeq == 0 {
			break
		}
		if minSeq <= since+1 {
			complete = true
			break
		}
		through = minSeq - 1
	}
	out := make([]sessionEvent, 0, len(seen))
	for _, ev := range seen {
		out = append(out, ev)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	if complete && len(out) > 0 {
		max := out[len(out)-1].Seq
		b.dshSet(ctx, func(s *dshState) {
			if max > s.LastEventSeq {
				s.LastEventSeq = max
			}
		})
	}
	return out, complete
}

// addToolFingerprints добавляет вызовы в историю (сквозь ходы) и проверяет цикл.
func (b *bot) addToolFingerprints(ctx context.Context, fps []string) (bool, int) {
	if len(fps) == 0 {
		return false, 0
	}
	var cycle bool
	var cycleLen int
	b.dshSet(ctx, func(st *dshState) {
		st.ToolSeq = append(st.ToolSeq, fps...)
		st.ToolSeq = trimToolSeq(st.ToolSeq, 100)
		cycleLen, cycle = detectCycle(st.ToolSeq)
	})
	return cycle, cycleLen
}

// recordAnswer запоминает ответ и меняет сессию при трёх похожих ответах подряд.
func (b *bot) recordAnswer(ctx context.Context, text string, epoch int64) {
	if b.dshCancelEpoch.Load() != epoch {
		return
	}
	var answers []string
	b.dshSet(ctx, func(st *dshState) {
		st.RecentAnswers = nextAnswerStreak(st.RecentAnswers, text)
		answers = append([]string(nil), st.RecentAnswers...)
	})
	if shouldReplaceAnswers(answers) {
		if err := b.replaceDSHSessionLocked(ctx, "повторяющиеся ответы", epoch); err != nil {
			log.Printf("maxbot: замена сессии после повторов: %v", err)
		}
	}
}

// ---- TTL модели ----

// runDSHModelTTL раз в 30 секунд сбрасывает модель после 2 часов простоя.
func (b *bot) runDSHModelTTL(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		b.dshSessionMu.Lock()
		st := b.dshSnapshot()
		if !modelExpired(st, time.Now(), time.Duration(b.dshCfg.ModelTTLHours)*time.Hour, b.dshCfg.Provider, b.dshCfg.DefaultModel) {
			b.dshSessionMu.Unlock()
			continue
		}
		applied := true
		if st.SessionID != "" {
			effort := ""
			if cat, err := b.dsh.modelCatalog(ctx); err == nil && cat.supportsEffort(b.dshCfg.Provider, b.dshCfg.DefaultModel, b.dshCfg.Effort) {
				effort = b.dshCfg.Effort
			}
			// Сначала применяем к живой сессии, потом фиксируем состояние: при
			// ошибке оставляем выбранную модель и повторим на следующем тике.
			if err := b.dsh.selectModel(ctx, st.SessionID, b.dshCfg.Provider, b.dshCfg.DefaultModel, effort); err != nil {
				log.Printf("maxbot: применение сброса модели: %v", err)
				applied = false
			}
		}
		if applied {
			b.dshSet(ctx, func(s *dshState) {
				s.Provider = b.dshCfg.Provider
				s.Model = b.dshCfg.DefaultModel
				s.Effort = b.dshCfg.Effort
				s.ModelSelectedAt = 0
				s.ModelLastAPIAt = time.Now().UnixMilli()
			})
		}
		b.dshSessionMu.Unlock()
		if applied {
			b.notifyOperator(ctx, fmt.Sprintf("Модель сброшена на %s (%d ч без запросов).", b.dshCfg.DefaultModel, b.dshCfg.ModelTTLHours))
		}
	}
}

// ---- отложенные задачи агента (schedule_create) ----

// runDSHScheduled периодически проверяет сессию, когда воркер свободен: если
// агент выполнил отложенную задачу (schedule_create), результат уже лежит в
// истории сессии, но нового промпта от оператора не было. Доставляем только
// видимый текст ответа, reasoning не отправляем.
func (b *bot) runDSHScheduled(ctx context.Context) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	lastBeat := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !b.dshEnabled() {
			continue
		}
		epoch := b.dshCancelEpoch.Load()
		// Если воркер занят ходом/восстановлением — не мешаем.
		if !b.dshPumpMu.TryLock() {
			continue
		}
		sessionBefore := b.dshSnapshot().SessionID
		reason := b.pollScheduled(ctx, epoch, &lastBeat)
		b.dshPumpMu.Unlock()
		if reason != "" {
			// Замену делаем без pumpMu: replace сам его возьмёт и проверит, что
			// сессия всё ещё та же.
			if err := b.replaceDSHSession(ctx, reason, epoch, sessionBefore); err != nil {
				b.notifyOperator(ctx, "Не удалось создать новую сессию: "+err.Error())
			}
		}
	}
}

// pollScheduled доставляет результат отложенного хода и обрабатывает вопрос.
// Возвращает причину замены сессии (пусто — замена не нужна).
// Вызывается под dshPumpMu; epoch — эпоха отмены на начало опроса.
func (b *bot) pollScheduled(ctx context.Context, epoch int64, lastBeat *time.Time) string {
	if b.dshCancelEpoch.Load() != epoch {
		return ""
	}
	st := b.dshSnapshot()
	if st.SessionID == "" {
		return ""
	}
	state, err := b.dsh.state(ctx, st.SessionID)
	if err != nil {
		log.Printf("maxbot: состояние dsh-сессии: %v", err)
		return ""
	}
	// state.Active остаётся true у живой сессии постоянно (в т.ч. с
	// зарегистрированными расписаниями) — это НЕ признак работающего хода.
	// Ориентируемся только на running и очередь входов.
	if state.Running || len(state.Queue) > 0 {
		// Агент реально работает (в т.ч. отложенная задача) — это активность,
		// окно модели продлевается; heartbeat не реже heartbeat_sec.
		b.touchAPI(ctx)
		if time.Since(*lastBeat) >= time.Duration(b.dshCfg.HeartbeatSec)*time.Second {
			b.notifyOperator(ctx, "Агент работает")
			*lastBeat = time.Now()
		}
		return ""
	}
	events, complete := b.fetchNewEvents(ctx, st.SessionID, state.Cursor)
	if !complete {
		// Ошибка/неполная выборка: курсоры не двигаем, повторим на следующем тике.
		return ""
	}
	if b.dshCancelEpoch.Load() != epoch {
		return ""
	}
	ti := analyzeEvents(events)
	if (ti.Question != "" || hasPendingQuestion(state.PendingInput)) &&
		!ti.ContextOverflow && !ti.MaxTokens {
		if q := b.handleInputRequired(ctx, st.SessionID, ti.Question, state.Cursor, epoch); q != "" {
			b.notifyOperator(ctx, q)
		}
		return ""
	}

	delivered, tail, lastEnd := finalAssistantTextEvents(events, st.LastAnswerSeq, st.PendingSchedText)
	text := delivered
	// Курсор двигаем до отправки (как воркер): при падении/рестарте уже
	// доставленное не дублируется. Двигаем только до последнего turn/end, чтобы
	// не потерять текст незавершённого хвоста.
	b.dshSet(ctx, func(s *dshState) {
		if lastEnd > 0 && lastEnd > s.LastAnswerSeq {
			s.LastAnswerSeq = lastEnd
		}
		s.PendingSchedText = tail
	})
	if text != "" {
		if b.dshCancelEpoch.Load() != epoch {
			return ""
		}
		if err := b.sendAnswerText(ctx, text); err != nil {
			log.Printf("maxbot: отправка отложенного ответа: %v", err)
		}
		b.touchAPI(ctx)
	}

	// Сбой/цикл в отложенном ходе — действуем как для обычного хода.
	cycle, cycleLen := b.addToolFingerprints(ctx, ti.ToolCalls)
	if ti.ContextOverflow || ti.MaxTokens || cycle {
		switch {
		case ti.MaxTokens:
			return "достигнут лимит вывода (max-tokens)"
		case cycle:
			return fmt.Sprintf("цикл tool/call (период %d, 3 повтора)", cycleLen)
		default:
			return "переполнение контекста"
		}
	}
	return ""
}

// ---- команды MAX ----

// handleDSHMessage принимает текст/вложения из MAX и ставит запрос в очередь.
func (b *bot) handleDSHMessage(ctx context.Context, up maxUpdate) {
	if !b.dshEnabled() || up.Message == nil {
		return
	}
	text := strings.TrimSpace(up.Message.Body.Text)
	if cmd, ok := dshCommand(text); ok {
		b.handleDSHCommand(ctx, cmd, up)
		return
	}
	if text == "" && len(up.Message.Body.Attachments) == 0 {
		return
	}
	if err := b.unblockAwaitingInput(ctx); err != nil {
		b.notifyOperator(ctx, "Не удалось снять зависший вопрос агента: "+err.Error()+". Попробуйте /cancel.")
		return
	}
	item := dshQueueItem{
		Kind:        "prompt",
		RequestID:   newUUID(),
		Text:        text,
		Attachments: up.Message.Body.Attachments,
		Epoch:       b.dshCancelEpoch.Load(),
		TS:          time.Now().UnixMilli(),
	}
	if err := pushDSHQueue(ctx, b.rdb, item); err != nil {
		log.Printf("maxbot: постановка dsh-запроса в очередь: %v", err)
		return
	}
	if b.dshBusy.Load() {
		if n := dshQueueLen(ctx, b.rdb); n > 0 {
			b.notifyOperator(ctx, "Принято, "+fmtAck(n))
		} else {
			b.notifyOperator(ctx, "Принято")
		}
	}
}

// dshCommand распознаёт только команды меню бота. Остальные «/...» — промпты.
func dshCommand(text string) (string, bool) {
	if !strings.HasPrefix(text, "/") {
		return "", false
	}
	first := strings.Fields(text)[0]
	if i := strings.IndexByte(first, '@'); i > 0 {
		first = first[:i]
	}
	switch first {
	case "/menu", "/model", "/cancel", "/new", "/continue", "/status":
		return first, true
	}
	return "", false
}

func (b *bot) handleDSHCommand(ctx context.Context, cmd string, up maxUpdate) {
	userID, chatID := b.requesterIDs(up)
	switch cmd {
	case "/menu":
		b.sendDSHMenu(ctx, userID, chatID)
	case "/model":
		b.sendModelMenu(ctx, userID, chatID)
	case "/cancel":
		b.dshCancelEpoch.Add(1)
		if st := b.dshSnapshot(); st.SessionID != "" {
			_ = b.dsh.cancel(ctx, st.SessionID)
			b.touchAPI(ctx)
		}
		_ = clearDSHQueue(ctx, b.rdb)
		b.dshSet(ctx, func(s *dshState) {
			s.PendingSchedText = ""
			s.AwaitingInput = false
			s.InputSeq = 0
		})
		b.respond(userID, chatID, "Отменено. Очередь очищена.")
	case "/new":
		b.dshCancelEpoch.Add(1)
		if st := b.dshSnapshot(); st.SessionID != "" {
			_ = b.dsh.cancel(ctx, st.SessionID)
			b.touchAPI(ctx)
		}
		_ = clearDSHQueue(ctx, b.rdb)
		b.dshSet(ctx, func(st *dshState) { st.SessionID = "" })
		if _, err := b.ensureDSHSession(ctx); err != nil {
			return
		}
		b.respond(userID, chatID, "Создана новая сессия.")
	case "/continue":
		if err := b.unblockAwaitingInput(ctx); err != nil {
			b.respond(userID, chatID, "Не удалось снять зависший вопрос агента: "+err.Error()+". Попробуйте /cancel.")
			return
		}
		item := dshQueueItem{Kind: "continue", RequestID: newUUID(), Epoch: b.dshCancelEpoch.Load(), TS: time.Now().UnixMilli()}
		if err := pushDSHQueue(ctx, b.rdb, item); err != nil {
			b.respond(userID, chatID, "Не удалось поставить продолжение в очередь: "+err.Error())
			return
		}
		b.respond(userID, chatID, "Продолжаю.")
	case "/status":
		st := b.dshSnapshot()
		session := st.SessionID
		if session == "" {
			session = "(нет)"
		}
		ttl := "—"
		if st.Provider != b.dshCfg.Provider || st.Model != b.dshCfg.DefaultModel {
			last := st.ModelLastAPIAt
			if st.ModelSelectedAt > last {
				last = st.ModelSelectedAt
			}
			if last > 0 {
				left := time.Duration(b.dshCfg.ModelTTLHours)*time.Hour - time.Since(time.UnixMilli(last))
				if left < 0 {
					left = 0
				}
				ttl = left.Round(time.Minute).String()
			}
		}
		b.respond(userID, chatID, fmt.Sprintf("Модель: %s/%s\nСессия: %s\nОчередь: %d\nДо сброса модели: %s",
			st.Provider, st.Model, session, dshQueueLen(ctx, b.rdb), ttl))
	}
}

// requesterIDs достаёт user_id/chat_id из обновления.
func (b *bot) requesterIDs(up maxUpdate) (string, string) {
	userID, chatID := "", ""
	if up.Message != nil {
		if up.Message.Sender.UserID != 0 {
			userID = fmt.Sprintf("%d", up.Message.Sender.UserID)
		}
		if up.Message.Recipient.ChatID != 0 {
			chatID = fmt.Sprintf("%d", up.Message.Recipient.ChatID)
		}
	}
	if userID == "" && up.User.UserID != 0 {
		userID = fmt.Sprintf("%d", up.User.UserID)
	}
	if chatID == "" && up.ChatID != 0 {
		chatID = fmt.Sprintf("%d", up.ChatID)
	}
	return userID, chatID
}

// sendDSHMenu отправляет основное меню бота кнопками.
func (b *bot) sendDSHMenu(ctx context.Context, userID, chatID string) {
	rows := [][]maxButton{
		{
			{Type: "callback", Text: "Модель", Payload: "cmd|model"},
			{Type: "callback", Text: "Статус", Payload: "cmd|status"},
		},
		{
			{Type: "callback", Text: "Отмена", Payload: "cmd|cancel"},
			{Type: "callback", Text: "Продолжить", Payload: "cmd|continue"},
		},
		{
			{Type: "callback", Text: "Новая сессия", Payload: "cmd|new"},
		},
	}
	att := maxOutAttachment{Type: "inline_keyboard", Payload: map[string]any{"buttons": rows}}
	if err := b.client.sendRich(ctx, userID, chatID, "Меню бота:", "", []maxOutAttachment{att}); err != nil {
		log.Printf("maxbot: меню бота: %v", err)
	}
}

// handleDSHMenuAction выполняет действие из кнопки основного меню.
func (b *bot) handleDSHMenuAction(ctx context.Context, action string, cb *maxCallback, chatID string) {
	switch action {
	case "model", "cancel", "continue", "new", "status":
	default:
		b.answerCallback(ctx, cb, "Неизвестная команда")
		return
	}
	var up maxUpdate
	up.ChatID, _ = strconv.ParseInt(chatID, 10, 64)
	up.User.UserID = cb.User.UserID
	b.handleDSHCommand(ctx, "/"+action, up)
	b.answerCallback(ctx, cb, "")
}

// sendModelMenu отправляет inline-клавиатуру со всеми доступными моделями.
const modelPageSize = 25

type modelEntry struct {
	provider string
	id       string
	name     string
}

// sendModelMenu открывает первую страницу меню моделей.
func (b *bot) sendModelMenu(ctx context.Context, userID, chatID string) {
	b.sendModelMenuPage(ctx, userID, chatID, 0)
}

// sendModelMenuPage отправляет страницу меню моделей (не более 25 кнопок +
// навигация), чтобы не превысить лимит строк inline-клавиатуры MAX.
func (b *bot) sendModelMenuPage(ctx context.Context, userID, chatID string, offset int) {
	cat, err := b.dsh.modelCatalog(ctx)
	if err != nil {
		b.respond(userID, chatID, "Не удалось получить каталог моделей: "+err.Error())
		return
	}
	b.touchAPI(ctx)
	var entries []modelEntry
	for _, g := range cat.Groups {
		for _, m := range g.Models {
			entries = append(entries, modelEntry{provider: g.ID, id: m.ID, name: m.Name})
		}
	}
	if len(entries) == 0 {
		if err := b.client.sendRich(ctx, userID, chatID, "Модели недоступны.", "", nil); err != nil {
			log.Printf("maxbot: меню моделей: %v", err)
		}
		return
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= len(entries) {
		offset = (len(entries) - 1) / modelPageSize * modelPageSize
	}
	end := offset + modelPageSize
	if end > len(entries) {
		end = len(entries)
	}

	counts := make(map[string]int)
	for _, e := range entries {
		counts[e.id]++
	}
	st := b.dshSnapshot()
	var rows [][]maxButton
	for _, e := range entries[offset:end] {
		label := e.name
		if label == "" {
			label = e.id
		}
		if counts[e.id] > 1 {
			label = shortProvider(e.provider) + " · " + label
		}
		if len([]rune(label)) > 118 {
			label = string([]rune(label)[:118])
		}
		if e.provider == st.Provider && e.id == st.Model {
			label += "  ✓"
		}
		rows = append(rows, []maxButton{{
			Type:    "callback",
			Text:    label,
			Payload: "model|" + e.provider + "|" + e.id,
		}})
	}
	var nav []maxButton
	if offset > 0 {
		prev := offset - modelPageSize
		if prev < 0 {
			prev = 0
		}
		nav = append(nav, maxButton{Type: "callback", Text: "◀ Назад", Payload: "models|" + strconv.Itoa(prev)})
	}
	if end < len(entries) {
		nav = append(nav, maxButton{Type: "callback", Text: "▶ Ещё", Payload: "models|" + strconv.Itoa(end)})
	}
	if len(nav) > 0 {
		rows = append(rows, nav)
	}
	att := maxOutAttachment{Type: "inline_keyboard", Payload: map[string]any{"buttons": rows}}
	text := fmt.Sprintf("Модели %d–%d из %d. Сейчас: %s/%s", offset+1, end, len(entries), st.Provider, st.Model)
	if err := b.client.sendRich(ctx, userID, chatID, text, "", []maxOutAttachment{att}); err != nil {
		log.Printf("maxbot: меню моделей: %v", err)
	}
}

// shortProvider — короткий префикс провайдера для неоднозначных имён моделей.
func shortProvider(id string) string {
	switch id {
	case "wbllm":
		return "wb"
	case "deepseek-official":
		return "ds"
	}
	return id
}

// handleDSHCallback обрабатывает выбор модели из inline-меню.
func (b *bot) handleDSHCallback(ctx context.Context, cb *maxCallback, chatID string) {
	if !b.dshEnabled() {
		b.answerCallback(ctx, cb, "Чат с агентом выключен")
		return
	}
	parts := strings.Split(cb.Payload, "|")
	if len(parts) == 2 && parts[0] == "cmd" {
		b.handleDSHMenuAction(ctx, parts[1], cb, chatID)
		return
	}
	if len(parts) == 2 && parts[0] == "models" {
		off, _ := strconv.Atoi(parts[1])
		b.sendModelMenuPage(ctx, strconv.FormatInt(cb.User.UserID, 10), chatID, off)
		b.answerCallback(ctx, cb, "")
		return
	}
	if len(parts) != 3 || parts[0] != "model" {
		b.answerCallback(ctx, cb, "Неизвестная команда")
		return
	}
	provider, model := parts[1], parts[2]
	cat, err := b.dsh.modelCatalog(ctx)
	if err != nil {
		b.answerCallback(ctx, cb, "Не удалось получить каталог моделей")
		return
	}
	if !cat.hasModel(provider, model) {
		b.answerCallback(ctx, cb, "Модель недоступна")
		return
	}
	effort := ""
	if cat.supportsEffort(provider, model, b.dshCfg.Effort) {
		effort = b.dshCfg.Effort
	}
	b.dshSessionMu.Lock()
	st := b.dshSnapshot()
	if st.SessionID == "" {
		_, err = b.createDSHSessionLocked(ctx, provider, model, effort)
	} else {
		err = b.dsh.selectModel(ctx, st.SessionID, provider, model, effort)
	}
	b.dshSessionMu.Unlock()
	if err != nil {
		b.answerCallback(ctx, cb, "Не удалось выбрать модель: "+err.Error())
		return
	}
	if st.SessionID != "" {
		now := time.Now().UnixMilli()
		b.dshSet(ctx, func(s *dshState) {
			s.Provider = provider
			s.Model = model
			s.Effort = effort
			s.ModelSelectedAt = now
			s.ModelLastAPIAt = now
		})
	}
	b.answerCallback(ctx, cb, fmt.Sprintf("Модель выбрана: %s/%s", provider, model))
}

func (b *bot) answerCallback(ctx context.Context, cb *maxCallback, text string) {
	if err := b.client.answerCallback(ctx, cb.CallbackID, text); err != nil {
		// Пустой ответ на callback MAX может отклонить; обычным сообщением его
		// не дублируем (иначе уйдёт пустой текст и снова 400).
		if text != "" {
			b.notifyOperator(ctx, text)
		}
	}
}

// ---- отправка ----

// sendAnswerText отправляет ответ: текст (Markdown с fallback) или файл > 4000.
func (b *bot) sendAnswerText(ctx context.Context, text string) error {
	if strings.TrimSpace(text) == "" {
		text = "Готово (агент не вернул текст)."
	}
	if len([]rune(text)) <= 4000 {
		return b.sendMarkdown(ctx, text)
	}
	ext := ".txt"
	if looksLikeMarkdown(text) {
		ext = ".md"
	}
	name := "agent-" + time.Now().Format("20060102-150405") + ext
	if err := b.client.sendFile(ctx, name, []byte(text), "Ответ агента (файл)"); err == nil {
		return nil
	} else {
		log.Printf("maxbot: отправка ответа файлом не удалась: %v", err)
	}
	// Fallback: режем на части, чтобы не потерять ответ.
	r := []rune(text)
	for i := 0; i < len(r); i += 3500 {
		end := i + 3500
		if end > len(r) {
			end = len(r)
		}
		if err := b.sendMarkdown(ctx, string(r[i:end])); err != nil {
			return err
		}
	}
	return nil
}

// sendMarkdown отправляет Markdown, при ошибке повторяет обычным текстом.
func (b *bot) sendMarkdown(ctx context.Context, text string) error {
	u, c := b.client.recipient()
	if u == "" && c == "" {
		return fmt.Errorf("адресат не задан")
	}
	if err := b.client.sendRich(ctx, u, c, text, "markdown", nil); err == nil {
		return nil
	}
	return b.client.sendRich(ctx, u, c, text, "", nil)
}

// notifyOperator отправляет служебное сообщение оператору.
func (b *bot) notifyOperator(ctx context.Context, text string) {
	if !b.client.hasRecipient() {
		return
	}
	if err := b.sendMarkdown(ctx, text); err != nil {
		log.Printf("maxbot: служебное сообщение в MAX: %v", err)
	}
}
