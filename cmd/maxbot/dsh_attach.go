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
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	maxImageBytes = 50 << 20  // 50 МБ — лимит изображения MAX/harness
	maxFileBytes  = 200 << 20 // практический предел для входящих файлов
)

// attachmentHTTP — отдельный клиент для скачивания вложений MAX.
var attachmentHTTP = &http.Client{Timeout: 180 * time.Second}

// collectAttachments скачивает вложения из MAX: изображения — в base64-блоки
// промпта, файлы — в /opt/dsh-bot/incoming/<дата>/ с путём в тексте промпта.
func (b *bot) collectAttachments(ctx context.Context, atts []maxAttachment) ([]promptPart, []string) {
	var images []promptPart
	var files []string
	for _, a := range atts {
		switch a.Type {
		case "image":
			if a.Payload.URL == "" {
				continue
			}
			data, mediaType, err := downloadBytes(ctx, a.Payload.URL, maxImageBytes)
			if err != nil {
				log.Printf("maxbot: скачивание изображения: %v", err)
				b.notifyOperator(ctx, "Не удалось скачать изображение из MAX: "+err.Error())
				continue
			}
			if mediaType == "" {
				mediaType = "image/jpeg"
			}
			images = append(images, promptPart{
				Type:      "image",
				MediaType: mediaType,
				Data:      base64.StdEncoding.EncodeToString(data),
			})
		case "file":
			if a.Payload.URL == "" {
				continue
			}
			name := safeFileName(a.Payload.Filename)
			path, err := b.saveIncomingFile(ctx, a.Payload.URL, name)
			if err != nil {
				log.Printf("maxbot: сохранение файла: %v", err)
				b.notifyOperator(ctx, "Не удалось сохранить файл из MAX: "+err.Error())
				continue
			}
			files = append(files, path)
		}
	}
	return images, files
}

// downloadBytes скачивает вложение в память с ограничением размера.
func downloadBytes(ctx context.Context, url string, limit int64) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := attachmentHTTP.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("http %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > limit {
		return nil, "", fmt.Errorf("файл больше %d МБ", limit>>20)
	}
	ct := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	return data, ct, nil
}

// saveIncomingFile сохраняет файл в incoming/<дата>/ и возвращает путь.
func (b *bot) saveIncomingFile(ctx context.Context, url, name string) (string, error) {
	dir := filepath.Join(b.dshCfg.IncomingDir, time.Now().Format("2006-01-02"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	now := time.Now()
	final := filepath.Join(dir, fmt.Sprintf("%s-%09d-%s", now.Format("150405"), now.Nanosecond(), name))
	tmp := final + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	defer func() {
		f.Close()
		os.Remove(tmp)
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := attachmentHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("http %d", resp.StatusCode)
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxFileBytes+1))
	if err != nil {
		return "", err
	}
	if n > maxFileBytes {
		return "", fmt.Errorf("файл больше %d МБ", maxFileBytes>>20)
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, final); err != nil {
		return "", err
	}
	return final, nil
}
