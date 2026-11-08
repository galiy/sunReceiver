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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxAPIBase — базовый URL API MAX (для ботов); токен передаётся только в
// заголовке Authorization. Переменная (не константа), чтобы тесты могли
// подставлять локальный httptest-сервер.
var maxAPIBase = "https://platform-api2.max.ru"

// Таймаут одного HTTP-запроса к API MAX (отправка/ответ подписчику).
const maxRequestTimeout = 5 * time.Second

// Long polling /updates: параметр timeout в запросе и клиентские дедлайны.
const maxUpdatesPollTimeout = 25 * time.Second
const maxUpdatesContextTimeout = 90 * time.Second
const maxUpdatesHTTPTimeout = maxUpdatesContextTimeout + 15*time.Second

// maxClient — клиент отправки сообщений в MAX (Bot API). Адресат может быть
// установлен как из конфига, так и позже при авто-регистрации (bot_started),
// поэтому поля адресата защищены мьютексом.
type maxClient struct {
	token string
	mu    sync.Mutex
	// Адресат: user_id (личный диалог) ИЛИ chat_id (чат/канал); приоритет user_id.
	userID string
	chatID string
	hc     *http.Client // обычный клиент (короткий таймаут) для отправки
	hcLong *http.Client // клиент для long polling /updates
	hcFile *http.Client // клиент для загрузки/скачивания файлов

	// sendMu сериализует отправки в MAX и выдерживает лимит 2 сообщения/сек.
	sendMu   sync.Mutex
	lastSend time.Time
}

func newMaxClient(token, userID, chatID string) *maxClient {
	return &maxClient{
		token:  token,
		userID: userID,
		chatID: chatID,
		hc:     &http.Client{Timeout: maxRequestTimeout},
		hcLong: &http.Client{Timeout: maxUpdatesHTTPTimeout},
		hcFile: &http.Client{Timeout: 120 * time.Second},
	}
}

// setRecipient задаёт адресата (вызывается при авто-регистрации).
func (m *maxClient) setRecipient(userID, chatID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.userID = userID
	m.chatID = chatID
}

// recipient возвращает текущих адресата.
func (m *maxClient) recipient() (userID, chatID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.userID, m.chatID
}

// hasRecipient — true, если адресат известен.
func (m *maxClient) hasRecipient() bool {
	u, c := m.recipient()
	return u != "" || c != ""
}

// send отправляет текстовое сообщение адресату (зарегистрированному подписчику).
func (m *maxClient) send(ctx context.Context, text string) error {
	u, c := m.recipient()
	return m.sendTo(ctx, u, c, text)
}

// sendTo отправляет текстовое сообщение конкретному получателю (user_id или chat_id).
func (m *maxClient) sendTo(ctx context.Context, userID, chatID, text string) error {
	return m.sendRich(ctx, userID, chatID, text, "", nil)
}

// maxOutAttachment — исходящее вложение MAX.
type maxOutAttachment struct {
	Type    string `json:"type"`
	Payload any    `json:"payload"`
}

// maxButton — кнопка inline-клавиатуры MAX.
type maxButton struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Payload string `json:"payload,omitempty"`
}

// sendRich отправляет сообщение с необязательным форматированием и вложениями.
// Отправки сериализуются и выдерживают лимит MAX (2 сообщения/сек в диалог).
func (m *maxClient) sendRich(ctx context.Context, userID, chatID, text, format string, attachments []maxOutAttachment) error {
	u := maxAPIBase + "/messages"
	q := url.Values{}
	if userID != "" {
		q.Set("user_id", userID)
	} else if chatID != "" {
		q.Set("chat_id", chatID)
	} else {
		return fmt.Errorf("max send: адресат не задан (user_id/chat_id)") // нет получателя
	}
	msg := map[string]any{}
	if text != "" {
		msg["text"] = text
	}
	if format != "" {
		msg["format"] = format
	}
	if len(attachments) > 0 {
		msg["attachments"] = attachments
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	m.sendMu.Lock()
	defer m.sendMu.Unlock()
	if wait := 550*time.Millisecond - time.Since(m.lastSend); wait > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	m.lastSend = time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u+"?"+q.Encode(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", m.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.hc.Do(req)
	if err != nil {
		return fmt.Errorf("max send: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("max send: http %d: %s", resp.StatusCode, string(b))
	}
	// Успех — 200. Дополнительно проверяем тело, чтобы не подтвердить запись при
	// ошибке в теле успешного ответа (например, ok:false / поле error).
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var probe map[string]any
	if err := json.Unmarshal(b, &probe); err == nil {
		if okf, exists := probe["ok"].(bool); exists && !okf {
			return fmt.Errorf("max send: ok=false (тело: %s)", string(b))
		}
		if es, _ := probe["error"].(string); es != "" {
			return fmt.Errorf("max send: error=%s (тело: %s)", es, string(b))
		}
	}
	return nil
}

// sameOrigin — совпадают ли схема+хост+порт двух URL.
func sameOrigin(a, b string) bool {
	ua, err := url.Parse(a)
	if err != nil {
		return false
	}
	ub, err := url.Parse(b)
	if err != nil {
		return false
	}
	return ua.Scheme == ub.Scheme && ua.Host == ub.Host
}

// maxBotCommand — команда бота для подсказок в MAX.
type maxBotCommand struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// setCommands регистрирует команды бота (PATCH /me/commands).
func (m *maxClient) setCommands(ctx context.Context, cmds []maxBotCommand) error {
	body, err := json.Marshal(map[string]any{"commands": cmds})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, maxAPIBase+"/me/commands", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", m.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.hc.Do(req)
	if err != nil {
		return fmt.Errorf("max commands: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("max commands: http %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// answerCallback отвечает на нажатие inline-кнопки (POST /answers).
func (m *maxClient) answerCallback(ctx context.Context, callbackID, text string) error {
	if callbackID == "" {
		return nil
	}
	payload := map[string]any{}
	if text != "" {
		payload["message"] = map[string]any{"text": text}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	u := maxAPIBase + "/answers?callback_id=" + url.QueryEscape(callbackID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", m.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.hc.Do(req)
	if err != nil {
		return fmt.Errorf("max answers: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("max answers: http %d", resp.StatusCode)
	}
	return nil
}

// uploadFile загружает файл в MAX и возвращает токен вложения.
func (m *maxClient) uploadFile(ctx context.Context, filename string, data []byte) (string, error) {
	// 1) Получаем URL загрузки и токен.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, maxAPIBase+"/uploads?type=file", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", m.token)
	resp, err := m.hcFile.Do(req)
	if err != nil {
		return "", fmt.Errorf("max uploads: %w", err)
	}
	rawUp, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("max uploads: http %d: %s", resp.StatusCode, truncate(string(rawUp), 300))
	}
	var up struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rawUp, &up); err != nil {
		return "", fmt.Errorf("max uploads: разбор ответа: %w", err)
	}
	if up.URL == "" {
		return "", fmt.Errorf("max uploads: пустой url загрузки")
	}

	// 2) Отправляем файл multipart-ом (поле data).
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="data"; filename="%s"`, filename))
	h.Set("Content-Type", "application/octet-stream")
	part, err := mw.CreatePart(h)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(data); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, up.URL, &buf)
	if err != nil {
		return "", err
	}
	// Токен отправляем только на тот же origin, что и API MAX: URL загрузки
	// может указывать на CDN/хранилище, куда токен бота уходить не должен.
	if sameOrigin(up.URL, maxAPIBase) {
		upReq.Header.Set("Authorization", m.token)
	}
	upReq.Header.Set("Content-Type", mw.FormDataContentType())
	upResp, err := m.hcFile.Do(upReq)
	if err != nil {
		return "", fmt.Errorf("max upload: %w", err)
	}
	raw, _ := io.ReadAll(io.LimitReader(upResp.Body, 1<<20))
	upResp.Body.Close()
	if upResp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("max upload: http %d: %s", upResp.StatusCode, truncate(string(raw), 300))
	}
	var uploaded struct {
		Token  string `json:"token"`
		Retval string `json:"retval"`
	}
	if json.Unmarshal(raw, &uploaded) == nil && uploaded.Token != "" {
		return uploaded.Token, nil
	}
	if up.Token != "" {
		return up.Token, nil
	}
	return "", fmt.Errorf("max upload: сервер не вернул token")
}

// sendFile загружает файл и отправляет его сообщением с подписью. Ошибка
// attachment.not.ready обрабатывается повтором с нарастающей паузой.
func (m *maxClient) sendFile(ctx context.Context, filename string, data []byte, caption string) error {
	token, err := m.uploadFile(ctx, filename, data)
	if err != nil {
		return err
	}
	att := []maxOutAttachment{{Type: "file", Payload: map[string]string{"token": token}}}
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(1<<attempt) * time.Second):
			}
		}
		u, c := m.recipient()
		lastErr = m.sendRich(ctx, u, c, caption, "", att)
		if lastErr == nil {
			return nil
		}
		if !strings.Contains(lastErr.Error(), "attachment.not.ready") &&
			!strings.Contains(lastErr.Error(), "not.processed") {
			return lastErr
		}
	}
	return lastErr
}

// ---- авто-регистрация адресата (первый подписчик бота) ----

// maxAttachment — вложение входящего сообщения MAX.
type maxAttachment struct {
	Type    string `json:"type"`
	Payload struct {
		URL      string `json:"url"`
		Token    string `json:"token"`
		Filename string `json:"filename"`
		Size     int64  `json:"size"`
	} `json:"payload"`
}

// maxMessage — сообщение во входящем обновлении (message_created).
type maxMessage struct {
	Sender struct {
		UserID int64 `json:"user_id"`
		IsBot  bool  `json:"is_bot"`
	} `json:"sender"`
	Recipient struct {
		ChatID int64 `json:"chat_id"`
	} `json:"recipient"`
	Body struct {
		Text        string          `json:"text"`
		Attachments []maxAttachment `json:"attachments"`
	} `json:"body"`
}

// maxCallback — нажатие inline-кнопки (message_callback).
type maxCallback struct {
	CallbackID string `json:"callback_id"`
	Payload    string `json:"payload"`
	User       struct {
		UserID int64 `json:"user_id"`
	} `json:"user"`
}

// maxUpdate — одно событие из GET /updates (объект Update в API MAX).
type maxUpdate struct {
	UpdateType string `json:"update_type"`
	ChatID     int64  `json:"chat_id"`
	User       struct {
		UserID    int64  `json:"user_id"`
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
	} `json:"user"`
	Message  *maxMessage  `json:"message"`
	Callback *maxCallback `json:"callback"`
}

// maxUpdatesResponse — тело ответа GET /updates.
type maxUpdatesResponse struct {
	Updates []maxUpdate `json:"updates"`
	Marker  int64       `json:"marker"`
}

// fetchUpdates делает длинный опрос GET /updates (long polling). types — список
// типов событий через запятую (пусто — все). marker — указатель на следующее
// обновление (0/null — последнее).
func (m *maxClient) fetchUpdates(ctx context.Context, marker int64, types string) ([]maxUpdate, int64, error) {
	u := maxAPIBase + "/updates"
	q := url.Values{}
	if types != "" {
		q.Set("types", types)
	}
	q.Set("timeout", strconv.Itoa(int(maxUpdatesPollTimeout/time.Second)))
	if marker > 0 {
		q.Set("marker", strconv.FormatInt(marker, 10))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u+"?"+q.Encode(), nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", m.token)
	resp, err := m.hcLong.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, 0, fmt.Errorf("max updates: http %d: %s", resp.StatusCode, string(b))
	}
	var out maxUpdatesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, 0, err
	}
	return out.Updates, out.Marker, nil
}

// subscriberEvents — события, которые отслеживаются для регистрации/отписки бота.
const subscriberEvents = "bot_started,bot_added,bot_stopped,bot_removed,dialog_removed,message_created,message_callback"

// isLongPollTimeout сообщает, что ошибка — срабатывание ИМЕННО нашего
// context-дедлайна запроса. Для /updates это штатное завершение пустого
// long-poll, а не сетевая ошибка.
func isLongPollTimeout(err error) bool {
	return err != nil && errors.Is(err, context.DeadlineExceeded)
}

// pollSubscriberRound — один цикл long-poll с трактовкой дедлайна как пустого
// опроса: timedOut=true, err=nil, next=0 (marker сохраняется вызывающим).
func (m *maxClient) pollSubscriberRound(ctx context.Context, marker int64, types string) (updates []maxUpdate, next int64, timedOut bool, err error) {
	updates, next, err = m.fetchUpdates(ctx, marker, types)
	if err != nil && isLongPollTimeout(err) {
		return nil, 0, true, nil
	}
	return updates, next, false, err
}

// runSubscriber — фоновый цикл сопровождения подписки бота:
//   - адресат не задан → первое подписка-событие (bot_started/bot_added, либо
//     написанное сообщение) фиксируется как адресат (записывается в конфиг);
//   - адресат задан → другие подписки игнорируются, а отписка (bot_stopped/
//     bot_removed/dialog_removed от текущего адресата) очищает адресат в конфиге
//     и возвращает в режим ожидания подписчика.
func (b *bot) runSubscriber(stop context.Context) {
	marker := loadMaxMarker(stop, b.rdb)
	var lastErr time.Time
	for {
		select {
		case <-stop.Done():
			return
		default:
		}
		ctx, cancel := context.WithTimeout(stop, maxUpdatesContextTimeout)
		updates, next, timedOut, err := b.client.pollSubscriberRound(ctx, marker, subscriberEvents)
		cancel()
		if stop.Err() != nil {
			return
		}
		if timedOut {
			// Пустой long-poll: сервер держал запрос дольше нашего дедлайна.
			// Не ошибка, marker не трогаем; короткая пауза и повтор.
			select {
			case <-stop.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		if err != nil {
			if errors.Is(err, context.Canceled) && stop.Err() != nil {
				return
			}
			if time.Since(lastErr) >= 30*time.Second {
				log.Printf("maxbot: опрос событий подписчика не удался: %v", err)
				lastErr = time.Now()
			}
			select {
			case <-stop.Done():
				return
			case <-time.After(10 * time.Second):
			}
			continue
		}
		if next > 0 {
			marker = next
		}
		for _, up := range updates {
			b.handleUpdate(stop, up)
		}
		// Marker сохраняем после обработки пачки: при сбое сообщения не теряются.
		saveMaxMarker(stop, b.rdb, marker)
		if len(updates) == 0 {
			select {
			case <-stop.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}
}

// handleSubscriberEvent обрабатывает одно событие подписки/отписки (совместимая
// обёртка для тестов).
func (b *bot) handleSubscriberEvent(up maxUpdate) {
	b.handleUpdate(context.Background(), up)
}

// handleUpdate обрабатывает одно событие из /updates.
func (b *bot) handleUpdate(ctx context.Context, up maxUpdate) {
	// 0 — отсутствующий id; FormatInt дал бы строку "0" и сломал бы проверки
	// «адресат задан»/выбор user_id vs chat_id.
	userID, chatID := "", ""
	if up.User.UserID != 0 {
		userID = strconv.FormatInt(up.User.UserID, 10)
	}
	if up.ChatID != 0 {
		chatID = strconv.FormatInt(up.ChatID, 10)
	}
	if up.Message != nil {
		if up.Message.Sender.UserID != 0 {
			userID = strconv.FormatInt(up.Message.Sender.UserID, 10)
		}
		if chatID == "" && up.Message.Recipient.ChatID != 0 {
			chatID = strconv.FormatInt(up.Message.Recipient.ChatID, 10)
		}
	}
	switch up.UpdateType {
	case "message_callback":
		if up.Callback == nil {
			return
		}
		uid := ""
		if up.Callback.User.UserID != 0 {
			uid = strconv.FormatInt(up.Callback.User.UserID, 10)
		}
		if !b.isAllowed(uid) {
			b.respond(uid, chatID, b.deniedMsg())
			return
		}
		if b.client.hasRecipient() && !b.isOurSubscriber(uid, chatID) {
			b.respond(uid, chatID, b.rejectedMsg())
			return
		}
		b.handleDSHCallback(ctx, up.Callback, chatID)
	case "bot_started", "bot_added":
		if !b.isAllowed(userID) {
			// Аккаунт вне белого списка — отказ, адресат не трогаем.
			b.respond(userID, chatID, b.deniedMsg())
			return
		}
		if !b.client.hasRecipient() && (userID != "" || chatID != "") {
			b.registerRecipient(userID, chatID)
			b.respond(userID, chatID, b.registeredMsg())
			return
		}
		if b.client.hasRecipient() && !b.isOurSubscriber(userID, chatID) {
			b.respond(userID, chatID, b.rejectedMsg())
		}
	case "bot_stopped", "bot_removed", "dialog_removed":
		if b.isOurSubscriber(userID, chatID) {
			b.clearRecipient()
		}
	case "message_created":
		if up.Message != nil && up.Message.Sender.IsBot {
			return // не отвечаем ботам: исключает цикл «ответ → ответ»
		}
		if !b.isAllowed(userID) {
			b.respond(userID, chatID, b.deniedMsg())
			return
		}
		if !b.client.hasRecipient() && (userID != "" || chatID != "") {
			b.registerRecipient(userID, chatID)
			b.respond(userID, chatID, b.registeredMsg())
		} else if b.client.hasRecipient() && !b.isOurSubscriber(userID, chatID) {
			b.respond(userID, chatID, b.rejectedMsg())
			return
		}
		b.handleDSHMessage(ctx, up)
	}
}

// isAllowed — входит ли user_id в белый список (allowed_user_ids).
// Пустой список — регистрация запрещена для всех.
func (b *bot) isAllowed(userID string) bool {
	for _, id := range b.cfg.AllowedUserIDs {
		if id == userID {
			return true
		}
	}
	return false
}

// registeredMsg — сообщение первому подписчику.
func (b *bot) registeredMsg() string {
	return "Вы зарегистрированы и будете получать сообщения с электростанции ⚡"
}

// rejectedMsg — сообщение последующим подписчикам (регистрация невозможна).
func (b *bot) rejectedMsg() string {
	return "Регистрация невозможна: подписка на уведомления уже привязана к другому пользователю."
}

// deniedMsg — сообщение аккаунту вне белого списка (allowed_user_ids).
func (b *bot) deniedMsg() string {
	return "Доступ запрещён: ваш аккаунт не в списке разрешённых получателей уведомлений."
}

// respond отправляет разовое сообщение конкретному получателю (автору события).
func (b *bot) respond(userID, chatID, text string) {
	ctx, cancel := context.WithTimeout(context.Background(), maxRequestTimeout)
	defer cancel()
	if err := b.client.sendTo(ctx, userID, chatID, text); err != nil {
		log.Printf("maxbot: ответ подписчику в MAX не отправлен: %v", err)
		return
	}
}

// isOurSubscriber — совпадает ли событие с текущим адресатом.
func (b *bot) isOurSubscriber(evUserID, evChatID string) bool {
	uid, cid := b.client.recipient()
	return (uid != "" && evUserID == uid) || (cid != "" && evChatID == cid)
}

// registerRecipient фиксирует первого подписчика. Адресат сначала best-effort
// сохраняется в конфиг (по уже заданному из файла — не перезаписывается), затем
// ставится в клиент и в b.cfg (in-memory — источник истины для отправки).
func (b *bot) registerRecipient(userID, chatID string) {
	if err := b.saveRecipientToConfig(userID, chatID); err != nil {
		log.Printf("maxbot: подписчик зарегистрирован (user_id=%s, chat_id=%s), но адресат не сохранён в %s: %v (после рестарта потребуется повторная подписка)",
			userID, chatID, b.cfgPath, err)
	} else {
		log.Printf("maxbot: зарегистрирован подписчик (user_id=%s, chat_id=%s), адресат сохранён в %s",
			userID, chatID, b.cfgPath)
	}
	b.client.setRecipient(userID, chatID)
	b.cfg.UserID = userID
	b.cfg.ChatID = chatID
}

// clearRecipient очищает адресат после отписки текущего подписчика и возвращает
// бот в режим ожидания нового подписчика.
func (b *bot) clearRecipient() {
	if err := b.clearRecipientFromConfig(); err != nil {
		log.Printf("maxbot: очистить адресата в конфиге не удалось: %v", err)
	} else {
		log.Printf("maxbot: подписчик отписался (bot_stopped/bot_removed) — адресат очищен, ожидаю нового подписчика")
	}
	b.client.setRecipient("", "")
	b.cfg.UserID = ""
	b.cfg.ChatID = ""
}

// saveRecipientToConfig дописывает адресата в maxbot.json и сохраняет файл
// (атомарно). Если адресат уже задан — ничего не меняет.
func (b *bot) saveRecipientToConfig(userID, chatID string) error {
	cfg := *b.cfg
	if cfg.UserID != "" || cfg.ChatID != "" {
		return nil
	}
	cfg.UserID = userID
	cfg.ChatID = chatID
	return b.writeConfig(&cfg)
}

// clearRecipientFromConfig обнуляет user_id/chat_id в maxbot.json.
func (b *bot) clearRecipientFromConfig() error {
	cfg := *b.cfg
	cfg.UserID = ""
	cfg.ChatID = ""
	return b.writeConfig(&cfg)
}

// writeConfig атомарно сохраняет конфиг в файл (через rename).
func (b *bot) writeConfig(cfg *config) error {
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := b.cfgPath + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, b.cfgPath); err != nil {
		return err
	}
	return os.Chmod(b.cfgPath, 0o600)
}
