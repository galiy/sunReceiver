CC      = gcc
CFLAGS  = -O2 -std=gnu99 -Wall -Wextra
TARGET  = bmslistener
SRC     = bmslistener.c

all: $(TARGET)

$(TARGET): $(SRC)
	$(CC) $(CFLAGS) -o $(TARGET) $(SRC)

clean:
	rm -f $(TARGET)

install: $(TARGET)
	install -m 0755 $(TARGET) /usr/sbin/$(TARGET)
	install -m 0644 bmslistener.service /etc/systemd/system/bmslistener.service
	systemctl daemon-reload
	systemctl enable bmslistener.service

.PHONY: all clean install