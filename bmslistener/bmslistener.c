/* bmslistener — пассивный слушатель ANT BMS (часть проекта sunReceiver).
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
 * bmslistener — пассивный слушатель ANT BMS.
 *
 * Задача: на ПАК «Малина» (Raspbian jessie, armv7l) слушать USB-serial
 * адаптеры PL2303, каждый из которых подключён к своей ANT BMS 22PHB и
 * вещает 140-байтные live-кадры (19200 8N1, маркер AA 55 AA FF, CRC валидна).
 * Оба адаптера подключены режимом ТОЛЬКО чтения (TX не задействован).
 *
 * Поведение:
 *   - раз в 30 с сканировать /dev/ttyUSB* на предмет новых адаптеров;
 *   - порты, занятые другими процессами, НЕ трогаем;
 *   - адаптер считается ANT BMS, если с него идёт непрерывный поток
 *     140-байтных кадров с валидным маркером и контрольной суммой;
 *   - если адаптер молчит 30 с — забываем его;
 *   - публикуем коллекцию состояний активных адаптеров в System V shared
 *     memory (ключ 2018, единый сегмент). Контракт формата (фиксированный,
 *     web-api писать под него): объект `{"updated":<epoch>,"devices":[{...},...]}`
 *     + терминатор "#EOF" (конвенция экосистемы mapd/mpptd, read_json.php
 *     читает shmop 2015/2016/2017).
 *
 * Ни записи в shm, ни чтения портов с другими демонами (mapd, mpptd,
 * batmond, usbscanner) не конфликтуют: свой ключ shm + чтение портов с
 * фильтром занятости по /proc.
 *
 * build: gcc -O2 -Wall -o bmslistener bmslistener.c
 *         zig cc -O2 -std=gnu99 -Wall -Wextra -target arm-linux-musleabihf \
 *              -static -DVERSION=\"<версия>\" -o bmslistener bmslistener.c
 * licence-free, standalone C99+POSIX.
 *
 * --version (или -V) печатает «bmslistener <VERSION>», где VERSION задаётся
 * макросом при сборке (по умолчанию "dev").
 */
#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <fcntl.h>
#include <errno.h>
#include <dirent.h>
#include <termios.h>
#include <time.h>
#include <signal.h>
#include <sys/types.h>
#include <sys/stat.h>
#include <sys/ipc.h>
#include <sys/shm.h>
#include <stdint.h>
#include <stdarg.h>

/* версия демона: задаётся -DVERSION=\"<версия>\" при сборке, иначе "dev" */
#ifndef VERSION
#define VERSION "dev"
#endif

/* ---------- лог в файл (systemd 215 на этой плате не собирает stderr в журнал) ---------- */
static void bms_log(const char *fmt, ...) {
    va_list ap;
    char line[1024];
    va_start(ap, fmt);
    vsnprintf(line, sizeof line, fmt, ap);
    va_end(ap);
    int fd = open("/tmp/bmslistener.log", O_WRONLY | O_CREAT | O_APPEND, 0644);
    if (fd >= 0) {
        (void)write(fd, line, strlen(line));
        close(fd);
    }
    /* дубль в stderr на случай фонового запуска из консоли */
    fputs(line, stderr);
}

/* ---------- конфигурация ---------- */
#define BAUD              B19200      /* скорость UART ANT BMS */
#define FRAME_LEN         140         /* длина live-кадра */
#define MARKER0           0xAA
#define MARKER1           0x55
#define MARKER2           0xAA
#define MARKER3           0xFF
#define PROBE_TIMEOUT_MS  7000        /* сколько ждать первый валидный кадр при пробе */
#define PROBE_BACKOFF     300         /* сек: не пробовать порт, проваливший пробу, раньше */
#define PROBE_BUDGET_SEC  21          /* сек: суммарное время проб за один scan (3 полных пробы) */
#define SILENCE_TIMEOUT   30          /* сек: адаптер молчит -> забыть */
#define SCAN_INTERVAL     30          /* сек: интервал сканирования портов */
#define PUBLISH_INTERVAL  1           /* сек: интервал публикации в shm */
#define READ_TIMEOUT_MS   20          /* sleep при отсутствии данных (анти-spin) */
#define RX_BUFLEN        4096        /* буфер приёма на адаптер */
#define SHM_KEY           2018        /* System V ключ нашей коллекции */
#define SHM_SIZE          32768       /* размер сегмента (32 уст-ва × ~560 Б ≈ 18 КБ) */
#define SHM_PERMS         0666

#define MAX_DEVS          32
#define DEVPATH_MAX       64

/* ---------- данные одного адаптера (последний валидный кадр) ---------- */
typedef struct {
    int           fd;                /* fd порта, -1 = не слушаем */
    char          dev[DEVPATH_MAX];  /* /dev/ttyUSBn */
    char          rxbuf[RX_BUFLEN];
    int           rxlen;
    int           pend;              /* позиция найденного маркера ещё не дочитанного кадра, -1 = нет */
    time_t        last_ok;           /* последний валидный кадр (epoch) */
    uint32_t      frames_ok;         /* счётчик валидных кадров */
    /* поля последнего кадра */
    double        cells_v[32];
    double        current_a;
    int           soc;
    double        capacity_ah;
    double        remaining_ah;
    double        temps[6];
    int           cell_count;
    int           charge_mos, discharge_mos, balancer;
    double        power_w;
    int           max_cell_idx, ecmin_idx;
    double        max_cell_v, min_cell_v, avg_cell_v;
    uint16_t      checksum;             /* расчётная CRC кадра */
} bmsdev_t;

/* ---------- прототипы ---------- */
static int  port_open(const char *dev);
static void port_config(int fd);
static int  port_in_use_by_other(const char *dev);
static int  dev_is_bms(const char *dev);
static void bms_reset(bmsdev_t *b);
static void bms_feed(bmsdev_t *d, const unsigned char *data, int n);
static int  bms_parse_frame(bmsdev_t *d, const unsigned char *f);
static uint16_t checksum_frame(const unsigned char *f);
static void publish_all(bmsdev_t *devs, int n);
static time_t now_ts(void);
static void millisleep(long ms);

static volatile sig_atomic_t g_stop = 0;
static void on_signal(int sig) { (void)sig; g_stop = 1; }

/* ---------- кэш «не BMS» для backoff-пробы: не гонять 7-сек пробу каждые 30 с ---------- */
#define MAX_BAD 64
static char        g_bad_dev[MAX_BAD][DEVPATH_MAX];
static time_t      g_bad_at[MAX_BAD];
static int         g_bad_n = 0;

static int probe_bad_present(const char *dev) {
    time_t now = now_ts();
    for (int i = 0; i < g_bad_n; i++)
        if (strcmp(g_bad_dev[i], dev) == 0)
            return (now - g_bad_at[i] < PROBE_BACKOFF) ? 1 : 0;
    return 0;
}
static void probe_bad_add(const char *dev) {
    for (int i = 0; i < g_bad_n; i++)
        if (strcmp(g_bad_dev[i], dev) == 0) { g_bad_at[i] = now_ts(); return; }
    if (g_bad_n < MAX_BAD) {
        snprintf(g_bad_dev[g_bad_n], DEVPATH_MAX, "%s", dev);
        g_bad_at[g_bad_n] = now_ts();
        g_bad_n++;
    }
}
static void probe_bad_clear(const char *dev) {
    for (int i = 0; i < g_bad_n; i++)
        if (strcmp(g_bad_dev[i], dev) == 0) {
            g_bad_dev[i][0] = 0; g_bad_at[i] = 0;
        }
}

/* ---------- время ---------- */
static time_t now_ts(void) { return time(NULL); }

static void millisleep(long ms) {
    struct timespec ts; ts.tv_sec = ms / 1000; ts.tv_nsec = (ms % 1000) * 1000000L;
    nanosleep(&ts, NULL);
}

/* ---------- занят ли порт другим процессом (по /proc) ---------- */
static int port_in_use_by_other(const char *dev) {
    struct stat st_dev, st_fd;
    if (stat(dev, &st_dev) != 0) return 1; /* нет такого — считаем занятым (устройство исчезло) */

    DIR *dp = opendir("/proc");
    if (!dp) return 1;
    struct dirent *ent;
    int in_use = 0;
    char pd[64], fdpath[256], target[256];
    while ((ent = readdir(dp)) && !in_use) {
        if (ent->d_name[0] < '0' || ent->d_name[0] > '9') continue; /* только PID */
        snprintf(pd, sizeof pd, "/proc/%s/fd", ent->d_name);
        DIR *fdd = opendir(pd);
        if (!fdd) continue;
        struct dirent *fe;
        while ((fe = readdir(fdd)) && !in_use) {
            if (fe->d_name[0] == '.') continue;
            snprintf(fdpath, sizeof fdpath, "%s/%s", pd, fe->d_name);
            ssize_t tl = readlink(fdpath, target, sizeof target - 1);
            if (tl < 0) continue;
            target[tl] = 0;
            if (stat(target, &st_fd) == 0 &&
                S_ISCHR(st_fd.st_mode) &&
                st_fd.st_rdev == st_dev.st_rdev) {
                in_use = 1; /* другой процесс держит открытым этот же device */
                break;
            }
        }
        closedir(fdd);
    }
    closedir(dp);
    return in_use;
}

/* ---------- открытие и настройка порта (только чтение, TX не трогаем) ---------- */
static int port_open(const char *dev) {
    /* Открываем O_RDWR, а не O_RDONLY: иначе tcsetattr() на PL2303 не применяет
       скорость (остаётся прежняя → передискретизация 19200→9600, кадры не
       собираются). Данные мы при этом НЕ пишем (TX аппаратно отключён), только
       читаем жёлтую линию. */
    int fd = open(dev, O_RDWR | O_NOCTTY | O_NONBLOCK);
    if (fd < 0) return -1;
    port_config(fd);
    return fd;
}

static void port_config(int fd) {
    struct termios t;
    /* Мягкая настройка: ставим скорость (19200) и raw-режим для чтения
       полезных байт. Намеренно НЕ делаем tcflush/сбросов входящего буфера и
       НЕ трогаем узел сверх необходимого — чтобы не «сбивать» пассивный
       поток родного дисплея на общем /dev/ttyUSB* (см. замечание в работе). */
    if (tcgetattr(fd, &t) != 0) { memset(&t, 0, sizeof t); }
    cfmakeraw(&t);
    t.c_cflag |= (CREAD | CS8);      /* обязательно приём включён */
    t.c_cflag &= ~(CLOCAL | CSTOPB | PARENB);
    cfsetispeed(&t, BAUD);
    cfsetospeed(&t, BAUD);
    t.c_cc[VMIN] = 0;                /* non-blocking-семантика чтения */
    t.c_cc[VTIME] = 0;
    tcsetattr(fd, TCSANOW, &t);
    /* проталкиваем настройку: дать драйверу адаптироваться к новой скорости */
    usleep(100 * 1000);
}

/* ---------- сброс структуры адаптера ---------- */
static void bms_reset(bmsdev_t *b) {
    if (b->fd >= 0) { close(b->fd); b->fd = -1; }
    b->rxlen = 0;
    b->pend = -1;
    b->last_ok = 0;
    memset(b->rxbuf, 0, sizeof b->rxbuf);
    memset(&b->cells_v, 0, sizeof b->cells_v);
    memset(&b->temps, 0, sizeof b->temps);
    b->current_a = 0; b->soc = 0; b->capacity_ah = 0; b->remaining_ah = 0;
    b->cell_count = 0; b->charge_mos = 0; b->discharge_mos = 0; b->balancer = 0;
    b->power_w = 0; b->max_cell_idx = 0; b->ecmin_idx = 0;
    b->max_cell_v = 0; b->min_cell_v = 0; b->avg_cell_v = 0; b->checksum = 0;
    b->frames_ok = 0;
}

/* ---------- средство для числа (милливольты при LI3) ---------- */
static uint16_t rd16be(const unsigned char *f, int off) { return (uint16_t)((f[off] << 8) | f[off + 1]); }
static uint32_t rd32be(const unsigned char *f, int off) {
    return ((uint32_t)f[off] << 24) | ((uint32_t)f[off + 1] << 16) |
           ((uint32_t)f[off + 2] << 8) | (uint32_t)f[off + 3];
}

/* Контрольная сумма кадра: сумма байтов 4..137 (по области таблицы),
 * модуль 65536 без знака, сверяется как 16-bit BE на offset 138. */
static uint16_t checksum_frame(const unsigned char *f) {
    uint32_t sum = 0;
    int i;
    for (i = 4; i < 138; i++) sum += f[i];
    return (uint16_t)sum;
}

/* Парсинг одного 140-байтного live-кадра (по offset-таблице our spec). */
static int bms_parse_frame(bmsdev_t *d, const unsigned char *f) {
    if (f[0] != MARKER0 || f[1] != MARKER1 || f[2] != MARKER2 || f[3] != MARKER3)
        return 0;
    uint16_t csum = checksum_frame(f);
    uint16_t frame_csum = rd16be(f, 138);
    if (csum != frame_csum) return 0;   /* CRC не сошлась — кадр битый, не учитываем */

    int cell_count = f[123];           /* число ячеек (16S) */
    if (cell_count < 1) cell_count = 1;
    if (cell_count > 32) cell_count = 32;
    d->cell_count = cell_count;

    for (int i = 0; i < 32; i++) {
        int off = 6 + i * 2;
        /* off = 6..68 (чётные), off+1 ≤ 69 < 140 — переполнение не грозит */
        d->cells_v[i] = rd16be(f, off) / 1000.0;   /* mV -> V */
    }

    int32_t cur_raw = (int32_t)rd32be(f, 70);
    d->current_a = cur_raw / 10.0;                  /* ×0.1 A */
    d->soc = f[74];
    d->capacity_ah   = rd32be(f, 75) / 1000000.0;  /* ua -> Ah */
    d->remaining_ah   = rd32be(f, 79) / 1000000.0;
    /* цикл (83) не публикуем */

    for (int i = 0; i < 6; i++) {
        int off = 91 + i * 2;
        d->temps[i] = (int16_t)rd16be(f, off);      /* °C int16 (signed: −ая возможны) */
    }

    d->charge_mos    = f[103];
    d->discharge_mos = f[104];
    d->balancer      = f[105];
    d->power_w       = (double)(int32_t)rd32be(f, 111); /* Current Power int32 */

    d->max_cell_idx  = f[115];
    d->max_cell_v    = rd16be(f, 116) / 1000.0;
    d->ecmin_idx     = f[118];
    d->min_cell_v    = rd16be(f, 119) / 1000.0;
    d->avg_cell_v    = rd16be(f, 121) / 1000.0;
    d->checksum      = csum;
    return 1;
}

/* ---------- поток байт в парсер (выделение 140-кадров из непрерывного потока) ---------- */
static void bms_feed(bmsdev_t *d, const unsigned char *data, int n) {
    int i;
    for (i = 0; i < n; i++) {
        if (d->rxlen >= RX_BUFLEN) {
            /* переполнение — сохраняем хвост от найденного маркера (недочитанный кадр),
               иначе сбрасываем окно */
            if (d->pend >= 0 && d->pend < d->rxlen) {
                int keep = d->rxlen - d->pend;
                memmove(d->rxbuf, d->rxbuf + d->pend, keep);
                d->rxlen = keep;
                d->pend = 0;
                if (d->rxlen >= RX_BUFLEN) { d->rxlen = 0; d->pend = -1; }
            } else {
                d->rxlen = 0; d->pend = -1;
            }
        }
        d->rxbuf[d->rxlen++] = (char)data[i];

        /* если ещё не нашли начало кадра — ищем маркер AA 55 AA FF по всему окну */
        if (d->pend < 0) {
            for (int j = 0; j + 3 < d->rxlen; j++) {
                if ((unsigned char)d->rxbuf[j]     == MARKER0 &&
                    (unsigned char)d->rxbuf[j + 1] == MARKER1 &&
                    (unsigned char)d->rxbuf[j + 2] == MARKER2 &&
                    (unsigned char)d->rxbuf[j + 3] == MARKER3) {
                    d->pend = j; /* позиция найденного маркера — не теряем между байтами */
                    break;
                }
            }
        }
        /* маркера нет — продолжаем копить */
        if (d->pend < 0) continue;

        /* кадр ещё не весь накопился — ждём */
        if (d->pend + FRAME_LEN > d->rxlen) continue;

        const unsigned char *fr = (const unsigned char *)&d->rxbuf[d->pend];
        if (bms_parse_frame(d, fr)) {
            d->last_ok = now_ts();
            d->frames_ok++;
        }
        /* выкидываем готовый кадр и всё, что до него (guard: src за границей при n=0 — UB) */
        int drop = d->pend + FRAME_LEN;
        if (d->rxlen > drop)
            memmove(d->rxbuf, d->rxbuf + drop, d->rxlen - drop);
        d->rxlen -= drop;
        d->pend = -1;
    }
}

/* ---------- проба порта: является ли он ANT BMS ----------
 * Открываем, настраиваем, ждём первый валидный 140-кадр за PROBE_TIMEOUT_MS.
 * Читаем прямым циклом (без select): на этой плате select()+O_NONBLOCK tty
 * не сигналит о накопленных данных, а блокирующий read() их отдаёт.
 * При успехе возвращает УЖЕ открытый fd (без повторного открытия, чтобы не
 * тревожить PL2303 лишними open/close); при неуспехе — закрывает и даёт -1. */
static int dev_is_bms(const char *dev) {
    int fd = port_open(dev);
    if (fd < 0) return -1;
    bmsdev_t t; memset(&t, 0, sizeof t); t.pend = -1; t.fd = fd;
    snprintf(t.dev, sizeof t.dev, "%s", dev);
    int got = 0;
    unsigned char buf[512];
    time_t t0 = time(NULL);
    while (!got && !g_stop && (time(NULL) - t0) < (time_t)(PROBE_TIMEOUT_MS / 1000)) {
        ssize_t r = read(fd, buf, sizeof buf);
        if (r > 0) {
            bms_feed(&t, buf, (int)r);
            if (t.frames_ok > 0) got = 1;
        } else {
            usleep(50 * 1000);
        }
    }
    int ok = (got && t.last_ok > 0);
    bms_log("probe %s: frames_ok=%u %s\n",
            dev, t.frames_ok, ok ? "OK (BMS)" : "failed");
    if (!ok) { close(fd); return -1; }
    return fd;
}

/* ---------- публикация коллекции в shm (ключ 2018, объект {"updated":...,"devices":[...]} + #EOF) ---------- */
static void publish_all(bmsdev_t *devs, int n) {
    static char *shm = NULL;
    static int shmid = -1;
    if (shmid < 0) {
        shmid = shmget(SHM_KEY, SHM_SIZE, IPC_CREAT | SHM_PERMS);
        if (shmid < 0) { bms_log("shmget: %s\n", strerror(errno)); return; }
        shm = shmat(shmid, NULL, 0);
        if (shm == (void *)-1) { bms_log("shmat: %s\n", strerror(errno)); shm = NULL; return; }
    }
    if (!shm) return;

    char buf[SHM_SIZE];
    int pos = 0;
    /* безопасное добавление строки в buf (не даёт переполнить при больших объёмах) */
    #define BUFADD(...) do { int _r = snprintf(buf + pos, sizeof buf - pos, __VA_ARGS__); \
                         if (_r < 0) { pos = (int)sizeof buf - 1; break; } \
                         pos += _r; if (pos >= (int)sizeof buf) { pos = (int)sizeof buf - 1; } } while (0)

    BUFADD("{\"updated\":%ld,\"devices\":[", (long)now_ts());
    int cnt = 0;
    for (int i = 0; i < n; i++) {
        bmsdev_t *d = &devs[i];
        if (d->fd < 0 || d->last_ok == 0 || d->frames_ok == 0) continue;
        /* активен только если не молчит */
        if (now_ts() - d->last_ok > SILENCE_TIMEOUT) continue;

        char ts_str[32];
        struct tm tmv; localtime_r(&d->last_ok, &tmv);
        strftime(ts_str, sizeof ts_str, "%H:%M:%S", &tmv);

        BUFADD("%s{\"deviceName\":\"AntBms %d A/h (%s)\",\"port\":\"%s\","
            "\"timestamp\":%ld,\"time\":\"%s\","
            "\"cell_count\":%d,\"cells_v\":[",
            (cnt ? "," : ""), (int)(d->capacity_ah + 0.5), d->dev, d->dev,
            (long)d->last_ok, ts_str, d->cell_count);
        for (int k = 0; k < 32 && k < d->cell_count; k++) {
            BUFADD("%s%.3f", (k ? "," : ""), d->cells_v[k]);
        }
        BUFADD("],");
        BUFADD("\"current_a\":%.2f,\"soc\":%d,"
            "\"capacity_ah\":%.2f,\"remaining_ah\":%.2f,"
            "\"temperatures_c\":[", d->current_a, d->soc, d->capacity_ah, d->remaining_ah);
        for (int k = 0; k < 6; k++) {
            BUFADD("%s%.0f", (k ? "," : ""), d->temps[k]);
        }
        BUFADD("],");
        BUFADD("\"charge_mos\":%d,\"discharge_mos\":%d,\"balancer\":%d,"
            "\"power_w\":%.1f,"
            "\"max_cell_idx\":%d,\"max_cell_v\":%.3f,"
            "\"min_cell_idx\":%d,\"min_cell_v\":%.3f,"
            "\"avg_cell_v\":%.3f,\"frames\":%u",
            d->charge_mos, d->discharge_mos, d->balancer, d->power_w,
            d->max_cell_idx, d->max_cell_v, d->ecmin_idx, d->min_cell_v,
            d->avg_cell_v, d->frames_ok);
        BUFADD("}");
        cnt++;
    }
    BUFADD("]}#EOF");
    #undef BUFADD

    memset(shm, 0, SHM_SIZE);
    memcpy(shm, buf, (size_t)pos);
    /* сегмент читается целиком (NUL-терминация после memset);
       "#EOF" — конвенция экосистемы mapd/mpptd. */
}

/* ---------- сканирование /dev/ttyUSB* ---------- */
static void scan_for_new(bmsdev_t *devs, int n) {
    DIR *dp = opendir("/dev");
    if (!dp) return;
    struct dirent *e;
    time_t probe_deadline = now_ts() + PROBE_BUDGET_SEC;
    while ((e = readdir(dp)) != NULL) {
        if (strncmp(e->d_name, "ttyUSB", 6) != 0) continue;
        /* d_name до 255 симв.; "/dev/"+имя+NUL должен уместиться в path[DEVPATH_MAX] */
        if (strlen(e->d_name) > DEVPATH_MAX - 6) continue;
        char path[DEVPATH_MAX];
        snprintf(path, sizeof path, "/dev/%s", e->d_name);

        /* уже слушаем? */
        int known = 0;
        for (int i = 0; i < n; i++) {
            if (devs[i].fd >= 0 && strcmp(devs[i].dev, path) == 0) { known = 1; break; }
            /* а также уже опознан, но молчит и подлежит очистке — всё равно считаем занятым слотом */
            if (devs[i].dev[0] && strcmp(devs[i].dev, path) == 0 && devs[i].fd < 0) { known = 1; break; }
        }
        if (known) continue;

        /* занят другим процессом — не трогаем */
        if (port_in_use_by_other(path)) {
            bms_log("scan: %s is in use by other, skip\n", path);
            continue;
        }

        /* недавно уже пробовали — молчит (backoff, чтобы не гонять 7-сек пробу каждые 30 с) */
        if (probe_bad_present(path)) continue;

        /* лимит суммарного времени проб за scan (молчащие порты не должны стапить цикл) */
        if (now_ts() >= probe_deadline) {
            bms_log("scan: probe budget exhausted, rest next cycle\n");
            break;
        }

        /* пробуем: ANT BMS? dev_is_bms возвращает уже открытый fd или -1 */
        int probe_fd = dev_is_bms(path);
        if (probe_fd < 0) {
            bms_log("scan: %s is NOT ant bms (probe failed)\n", path);
            probe_bad_add(path);
            continue;
        }

        /* находим свободный слот в devs[] */
        int slot = -1;
        for (int i = 0; i < n; i++) if (devs[i].fd < 0 && devs[i].dev[0] == 0) { slot = i; break; }
        if (slot < 0) {
            bms_log("no free slot for %s\n", path);
            close(probe_fd);
            probe_bad_add(path); /* не влезло — отложим повторную пробу на PROBE_BACKOFF */
            break;
        }
        probe_bad_clear(path);

        bmsdev_t *b = &devs[slot];
        bms_reset(b);
        snprintf(b->dev, sizeof b->dev, "%s", path);
        b->fd = probe_fd; /* принимаем fd от probe — без повторного открытия */
        b->last_ok = now_ts();
        bms_log("bmslistener: added %s (slot %d)\n", path, slot);
    }
    closedir(dp);
}

/* ---------- чтение и обслуживание слушаемых портов ----------
 * Читаем напрямую (без select: на этой плате select()+O_NONBLOCK tty не
 * сигналит о накопленных данных). Порт открыт O_NONBLOCK, VMIN=0/VTIME=0 —
 * read отдаёт доступные байты без блокировки. */
static void service_fds(bmsdev_t *devs, int n) {
    int had_data = 0;
    unsigned char buf[512];
    for (int i = 0; i < n; i++) {
        bmsdev_t *d = &devs[i];
        if (d->fd < 0) continue;
        /* вычитываем всё, что пришло */
        for (;;) {
            ssize_t r = read(d->fd, buf, sizeof buf);
            if (r > 0) {
                had_data = 1;
                bms_feed(d, buf, (int)r);
            } else if (r == 0) {
                /* VMIN=0/VTIME=0: 0 = нет данных в данный момент, НЕ EOF.
                   Порт оставляем открытым, переходим к следующему. */
                break;
            } else if (errno == EAGAIN || errno == EWOULDBLOCK) {
                break; /* данных нет — следующий порт */
            } else {
                bms_log("bmslistener: read %s: %s, forgetting\n", d->dev, strerror(errno));
                probe_bad_add(d->dev); /* сдохший адаптер не гонять 7-сек пробой каждые 30 с */
                bms_reset(d); d->dev[0] = 0;
                break;
            }
        }
    }
    /* не было данных ни с одного порта — спим, чтобы не крутить цикл вхолостую
       (иначе при подключённых устройствах был бы busy-loop 100% CPU). */
    if (!had_data) millisleep(READ_TIMEOUT_MS);
}

/* ---------- очистка замолчавших ---------- */
static void reap_silent(bmsdev_t *devs, int n) {
    time_t now = now_ts();
    for (int i = 0; i < n; i++) {
        bmsdev_t *d = &devs[i];
        if (d->fd >= 0 && d->last_ok > 0 && (now - d->last_ok) > SILENCE_TIMEOUT) {
            bms_log("bmslistener: %s silent %lds, forgetting\n", d->dev, (long)(now - d->last_ok));
            bms_reset(d); d->dev[0] = 0;
        }
    }
}

/* ---------- main ---------- */
int main(int argc, char **argv) {
    /* --version / -V: печать версии и выход (без инициализации портов/shm) */
    if (argc == 2 && (strcmp(argv[1], "--version") == 0 || strcmp(argv[1], "-V") == 0)) {
        printf("bmslistener %s\n", VERSION);
        return 0;
    }

    signal(SIGTERM, on_signal);
    signal(SIGINT, on_signal);
    signal(SIGHUP, SIG_IGN);

    /* daemonize без инициализации терминала: работаем как systemd Type=simple */
    bms_log("bmslistener: starting (pid %d)\n", (int)getpid());

    bmsdev_t devs[MAX_DEVS];
    memset(devs, 0, sizeof devs);
    for (int i = 0; i < MAX_DEVS; i++) { devs[i].fd = -1; devs[i].dev[0] = 0; devs[i].pend = -1; }

    time_t next_scan = 0, next_pub = 0;
    while (!g_stop) {
        time_t now = now_ts();

        /* периодическое сканирование */
        if (now >= next_scan) {
            scan_for_new(devs, MAX_DEVS);
            next_scan = now + SCAN_INTERVAL;
        }

        /* чтение всех слушаемых портов (неблокирующе) */
        service_fds(devs, MAX_DEVS);

        /* забываем замолчавших */
        reap_silent(devs, MAX_DEVS);

        /* публикация */
        if (now >= next_pub) {
            publish_all(devs, MAX_DEVS);
            next_pub = now + PUBLISH_INTERVAL;
        }
    }

    for (int i = 0; i < MAX_DEVS; i++) if (devs[i].fd >= 0) { close(devs[i].fd); devs[i].fd = -1; }
    bms_log("bmslistener: stopped\n");
    return 0;
}