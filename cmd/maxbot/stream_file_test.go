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
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveStreamFileB64(t *testing.T) {
	ev := notifyEvent{FileName: "../dir/report.txt", FileB64: base64.StdEncoding.EncodeToString([]byte("hello"))}
	name, data, err := resolveStreamFile(ev, "", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if name != "report.txt" {
		t.Fatalf("name=%q, ожидали базовое имя", name)
	}
	if string(data) != "hello" {
		t.Fatalf("data=%q", data)
	}
}

func TestResolveStreamFileB64Size(t *testing.T) {
	ev := notifyEvent{FileName: "a.bin", FileB64: base64.StdEncoding.EncodeToString(make([]byte, 101))}
	if _, _, err := resolveStreamFile(ev, "", 100); err == nil {
		t.Fatal("ожидали ошибку предела размера")
	}
}

func TestResolveStreamFileBothOrNone(t *testing.T) {
	if _, _, err := resolveStreamFile(notifyEvent{FileName: "a", FileB64: "AA==", FilePath: "/tmp/a"}, "", 1024); err == nil {
		t.Fatal("ожидали ошибку при двух источниках")
	}
	if _, _, err := resolveStreamFile(notifyEvent{FileName: "a"}, "", 1024); err == nil {
		t.Fatal("ожидали ошибку без источника")
	}
	if _, _, err := resolveStreamFile(notifyEvent{FileB64: "AA=="}, "", 1024); err == nil {
		t.Fatal("ожидали ошибку без file_name")
	}
}

func TestResolveStreamFilePath(t *testing.T) {
	dir := t.TempDir()
	inside := filepath.Join(dir, "ok.txt")
	if err := os.WriteFile(inside, []byte("file-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	name, data, err := resolveStreamFile(notifyEvent{FileName: "ok.txt", FilePath: inside}, dir, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if name != "ok.txt" || string(data) != "file-data" {
		t.Fatalf("name=%q data=%q", name, data)
	}

	outside := filepath.Join(t.TempDir(), "evil.txt")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveStreamFile(notifyEvent{FileName: "evil.txt", FilePath: outside}, dir, 1024); err == nil {
		t.Fatal("ожидали отказ для пути вне files_dir")
	}

	if _, _, err := resolveStreamFile(notifyEvent{FileName: "ok.txt", FilePath: inside}, "", 1024); err == nil {
		t.Fatal("ожидали отказ без files_dir")
	}
	if _, _, err := resolveStreamFile(notifyEvent{FileName: "ok.txt", FilePath: "ok.txt"}, dir, 1024); err == nil ||
		!strings.Contains(err.Error(), "абсолютным") {
		t.Fatalf("ожидали требование абсолютного пути, err=%v", err)
	}
}
