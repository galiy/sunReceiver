VERSION ?= dev

BLDFLAGS_LINUX := -X main.version=$(VERSION)
BLDFLAGS_WIN   := -H windowsgui -X main.version=$(VERSION)
# bmslistener / mapgateway — C-демоны: версия зашивается макросом VERSION как строка-литерал.
BLDFLAGS_BMS   := -DVERSION=\"$(VERSION)\"
BLDFLAGS_MAPGW := -DVERSION=\"$(VERSION)\"

LINUX_ART := dist/sunReceiver-linux-amd64-$(VERSION)
WIN_ART   := dist/sunReceiver-windows-amd64-$(VERSION).exe
BMS_ART   := dist/bmslistener-armv7l-$(VERSION)
MAPGW_ART := dist/mapgateway-armv7l-$(VERSION)

# guard на внешние утилиты: zip нужен только релизу (Windows-архив),
# zig — только bmslistener (кросс-сборка ARM).
ZIP_CMD := $(shell command -v zip 2>/dev/null)
ZIG_CMD := $(shell command -v zig 2>/dev/null)

.PHONY: all linux-x64 win-x64 bmslistener mapgateway release clean dist test vet

all: linux-x64 win-x64 bmslistener mapgateway vet test

dist:
	mkdir -p dist

test:
	go test ./...

vet:
	go vet ./...

# Явная сборка linux-x64. N37: сначала удаляем устаревший артефакт (смена
# VERSION), чтобы старый файл не маскировал новый, затем собираем заново.
linux-x64: dist
	rm -f $(LINUX_ART)
	$(MAKE) VERSION=$(VERSION) $(LINUX_ART)

win-x64: dist
	rm -f $(WIN_ART)
	$(MAKE) VERSION=$(VERSION) $(WIN_ART)

# bmslistener (ARM, кросс-сборка) требует zig. N44: при его отсутствии — понятное
# предупреждение, а не падение: linux-x64/win-x64 собираются в любом случае.
bmslistener: dist
	@if [ -z "$(ZIG_CMD)" ]; then \
		echo "WARN: zig не найден (command -v zig) — пропускаю сборку $(BMS_ART)"; \
	else \
		rm -f $(BMS_ART); \
		$(MAKE) VERSION=$(VERSION) $(BMS_ART); \
	fi

# mapgateway (ARM, кросс-сборка) — шлюз Modbus TCP<->RTU для МАП. Требует zig.
mapgateway: dist
	@if [ -z "$(ZIG_CMD)" ]; then \
		echo "WARN: zig не найден (command -v zig) — пропускаю сборку $(MAPGW_ART)"; \
	else \
		rm -f $(MAPGW_ART); \
		$(MAKE) VERSION=$(VERSION) $(MAPGW_ART); \
	fi

# Артефактные цели (пересобираются, только если исходники новее артефакта и
# артефакт отсутствует/устарел). Здесь НЕТ фантомных зависимостей (dist/vet/test),
# иначе make всегда считал бы артефакт устаревшим и пересобирал бы его. На эти
# цели опирается release — чтобы НЕ пересобирать уже собранные бинарники (N38).
GO_SRC := $(wildcard *.go) go.mod go.sum

$(LINUX_ART): $(GO_SRC)
	mkdir -p dist
	rm -f $(LINUX_ART)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(BLDFLAGS_LINUX)" -o $(LINUX_ART) .
	@echo "OK: $(LINUX_ART)"

$(WIN_ART): $(GO_SRC)
	mkdir -p dist
	rm -f $(WIN_ART)
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "$(BLDFLAGS_WIN)" -o $(WIN_ART) .
	@echo "OK: $(WIN_ART)"

$(BMS_ART): bmslistener/bmslistener.c
	@if [ -z "$(ZIG_CMD)" ]; then \
		echo "ERROR: zig не найден (command -v zig) — не могу собрать $(BMS_ART)"; \
		exit 1; \
	fi
	mkdir -p dist
	rm -f $(BMS_ART)
	zig cc -O2 -std=gnu99 -Wall -Wextra -target arm-linux-musleabihf -static \
		$(BLDFLAGS_BMS) -o $(BMS_ART) bmslistener/bmslistener.c
	@echo "OK: $(BMS_ART)"

$(MAPGW_ART): mapgateway/mapgateway.c
	@if [ -z "$(ZIG_CMD)" ]; then \
		echo "ERROR: zig не найден (command -v zig) — не могу собрать $(MAPGW_ART)"; \
		exit 1; \
	fi
	mkdir -p dist
	rm -f $(MAPGW_ART)
	zig cc -O2 -std=gnu99 -Wall -Wextra -target arm-linux-musleabihf -static \
		$(BLDFLAGS_MAPGW) -o $(MAPGW_ART) mapgateway/mapgateway.c
	@echo "OK: $(MAPGW_ART)"

# Релиз. N38: использует уже собранные $(LINUX_ART)/$(WIN_ART), а не пересобирает
# их (зависит от артефактных целей, а не от явных linux-x64/win-x64), кладёт
# LICENSE и пакует. zip — только для Windows-архива: N36, guard на наличие zip.
release: $(LINUX_ART) $(WIN_ART) vet test
	@if [ -z "$(ZIP_CMD)" ]; then \
		echo "ERROR: zip не найден (command -v zip) — не могу собрать dist/sunReceiver-windows-amd64-$(VERSION).zip"; \
		exit 1; \
	fi
	rm -rf dist/.stage-release-$(VERSION)
	mkdir -p dist/.stage-release-$(VERSION)
	cp $(LINUX_ART) dist/.stage-release-$(VERSION)/sunReceiver
	cp $(WIN_ART) dist/.stage-release-$(VERSION)/sunReceiver.exe
	cp sunReceiver.sample.json dist/.stage-release-$(VERSION)/sunReceiver.json
	cp LICENSE dist/.stage-release-$(VERSION)/LICENSE
	tar -czf dist/sunReceiver-linux-amd64-$(VERSION).tar.gz -C dist/.stage-release-$(VERSION) sunReceiver sunReceiver.json LICENSE
	zip -j dist/sunReceiver-windows-amd64-$(VERSION).zip dist/.stage-release-$(VERSION)/sunReceiver.exe dist/.stage-release-$(VERSION)/sunReceiver.json dist/.stage-release-$(VERSION)/LICENSE
	rm -rf dist/.stage-release-$(VERSION)
	@echo "OK: dist/sunReceiver-linux-amd64-$(VERSION).tar.gz"
	@echo "OK: dist/sunReceiver-windows-amd64-$(VERSION).zip"

clean:
	rm -rf dist