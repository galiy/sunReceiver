/* read_bms — CGI (замена read_bms.php) для uhttpd на OpenWrt.
 *
 * sunReceiver
 * Copyright (C) 2026  Aleksandr Galinskii
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <https://www.gnu.org/licenses/>.
 *
 * Назначение: аналог web/read_bms.php без PHP. Читает System V shared memory
 * (ключ 2018, 32 КБ), куда bmslistener публикует JSON вида
 * {"updated":<epoch>,"devices":[...]} + терминатор "#EOF", и отдаёт его как
 * application/json. Если сегмента/терминатора нет — {"updated":0,"devices":[]}.
 *
 * uhttpd запускает этот бинарник как CGI через `list interpreter
 * ".php=/usr/sbin/read_bms"`, поэтому URL остаётся /read_bms.php.
 *
 * build (OpenWrt mips):
 *   mips-openwrt-linux-gcc -O2 -std=gnu99 -Wall -Wextra -o read_bms read_bms.c
 */

#include <stdio.h>
#include <string.h>
#include <sys/ipc.h>
#include <sys/shm.h>

#define SHM_KEY   2018
#define SHM_SIZE  32768
#define SHM_TERM  "#EOF"
#define SHM_TERM_LEN 4

static const char EMPTY_JSON[] = "{\"updated\":0,\"devices\":[]}";

static void emit_headers(void)
{
	fputs("Content-Type: application/json; charset=utf-8\r\n", stdout);
	fputs("Cache-Control: no-store\r\n", stdout);
	fputs("\r\n", stdout);
}

/* Первое вхождение подстроки term в buf[0..n); NULL, если нет. */
static char *find_sub(char *buf, size_t n, const char *term, size_t tlen)
{
	if (n < tlen)
		return NULL;
	for (size_t i = 0; i + tlen <= n; i++) {
		if (buf[i] == term[0] && memcmp(buf + i, term, tlen) == 0)
			return buf + i;
	}
	return NULL;
}

int main(void)
{
	emit_headers();

	int shmid = shmget(SHM_KEY, SHM_SIZE, 0);
	if (shmid < 0) {
		fputs(EMPTY_JSON, stdout);
		return 0;
	}

	char *shm = shmat(shmid, NULL, SHM_RDONLY);
	if (shm == (char *)-1) {
		fputs(EMPTY_JSON, stdout);
		return 0;
	}

	char *eof = find_sub(shm, SHM_SIZE, SHM_TERM, SHM_TERM_LEN);
	if (eof == NULL || eof == shm) {
		shmdt(shm);
		fputs(EMPTY_JSON, stdout);
		return 0;
	}

	fwrite(shm, 1, (size_t)(eof - shm), stdout);
	shmdt(shm);
	return 0;
}
