CC      = gcc
CFLAGS  = -O2 -std=gnu99 -Wall -Wextra
TARGET  = bmslistener
SRC     = bmslistener.c

# Веб-корень ПАК «Малина» (rw-раздел /settings, см. docs/malina-web-api.md).
WEBROOT     ?= /settings/html
WEBTARGET    = read_bms.php
WEBUSER      = www-data

all: $(TARGET)

$(TARGET): $(SRC)
	$(CC) $(CFLAGS) -o $(TARGET) $(SRC)

clean:
	rm -f $(TARGET)

# Устанавливает только веб-скрипт read_bms.php (web-api). Можно перевызывать
# без пересборки C-демона и без перезапуска сервиса.
install-web: web/$(WEBTARGET)
	install -m 0644 -o $(WEBUSER) -g $(WEBUSER) web/$(WEBTARGET) $(WEBROOT)/$(WEBTARGET)

install: $(TARGET) install-web
	install -m 0755 $(TARGET) /usr/sbin/$(TARGET)
	install -m 0644 bmslistener.service /etc/systemd/system/bmslistener.service
	systemctl daemon-reload
	systemctl enable bmslistener.service

uninstall:
	rm -f /usr/sbin/$(TARGET) /etc/systemd/system/bmslistener.service \
		$(WEBROOT)/$(WEBTARGET)
	systemctl daemon-reload

.PHONY: all clean install install-web uninstall