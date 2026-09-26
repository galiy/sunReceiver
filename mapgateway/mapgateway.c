/*
 * mapgateway — шлюз Modbus TCP <-> Modbus RTU (USB) для МАП Титанатор.
 * Часть проекта sunReceiver.
 *
 * Назначение: держит открытым USB-COM МАП в режиме ModBus RTU (например
 * /dev/serial/by-id/usb-FTDI_... или /dev/ttyUSBn, 115200 8N1) и слушает
 * TCP-порт (по умолчанию 502), транслируя запросы Modbus TCP (MBAP) в кадры
 * Modbus RTU (unit + PDU + CRC16) и обратно. Unit адрес (адрес МАП) —
 * прозрачный: берётся из MBAP-заголовка запроса.
 *
 * Совместим как замена прямого MАП-гейта 192.0.2.74:502: sunReceiver
 * строит адрес "<ip>:502" (main.go, modbusmap.DefaultPort=502) и использует
 * функцию 0x03 с побайтовой адресацией ячеек MAP — шлюз это транслирует
 * без изменений, поэтому клиенту достаточно сменить ip в конфиге.
 *
 * Сборка (armv7, статически):
 *   zig cc -O2 -std=gnu99 -Wall -Wextra -target arm-linux-musleabihf -static \
 *       -DVERSION='"1.0.0"' -o mapgateway mapgateway/mapgateway.c
 *
 * Использование:
 *   mapgateway [-d device] [-b baud] [-p tcp_port] [-l listen_addr] [-v]
 *   device — путь к COM; по умолчанию автопоиск среди свободных
 *            ttyUSB, ttyACM и serial-by-id в /dev с проверкой
 *            идентификации МАП (по одному кандидату за проход)
 *   baud   — 9600|19200|38400|57600|115200 (по умолчанию 115200)
 *   tcp_port — по умолчанию 502
 *   --version / -V — печатает "mapgateway <VERSION>" и выходит
 */

#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <strings.h>
#include <stdarg.h>
#include <stdint.h>
#include <errno.h>
#include <unistd.h>
#include <fcntl.h>
#include <termios.h>
#include <signal.h>
#include <poll.h>
#include <time.h>
#include <sys/types.h>
#include <sys/stat.h>
#include <dirent.h>
#include <glob.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <netinet/in.h>
#include <netinet/tcp.h>
#include <arpa/inet.h>

#ifndef VERSION
#define VERSION "dev"
#endif

#define DEF_TCP_PORT 502
#define DEF_LISTEN "0.0.0.0"
#define DEVPATH_MAX  256
#define PROBE_MS     400      /* короткий опрос устройства при поиске */

#define MAX_CLIENTS   16
#define MAX_FRAME     512      /* MBAP + PDU максимум */
#define CLIENT_OUTBUF (MAX_FRAME * 4)  /* очередь недописанного ответа клиенту */
#define RTU_MAX       260
#define CLIENT_IDLE_MS 300000ull       /* 5 мин без обмена — слот освобождается */
#define LOG_PRI       30       /* facility daemon(3)*8 + severity info(6) = 30 */

/* ---------------- лог: syslog-уведомления в /dev/log (rsyslog -> .253) ------ */
static int  g_verbose = 0;
static int  g_syslog_fd = -1;

static void syslog_send(const char *line)
{
    if (g_syslog_fd < 0) {
        g_syslog_fd = socket(AF_UNIX, SOCK_DGRAM, 0);
        if (g_syslog_fd >= 0) {
            struct sockaddr_un sa;
            memset(&sa, 0, sizeof(sa));
            sa.sun_family = AF_UNIX;
            strncpy(sa.sun_path, "/dev/log", sizeof(sa.sun_path) - 1);
            if (connect(g_syslog_fd, (struct sockaddr *)&sa, sizeof(sa)) != 0) {
                close(g_syslog_fd);
                g_syslog_fd = -1;
            }
        }
    }
    if (g_syslog_fd >= 0) {
        char out[512];
        int n = snprintf(out, sizeof(out), "<%d>%s", LOG_PRI, line);
        send(g_syslog_fd, out, (size_t)n, 0);
    }
    if (g_verbose)
        fputs(line, stderr);
}

#define GW_LOG(...) do {                                   \
        char _b[400];                                      \
        snprintf(_b, sizeof(_b), "mapgateway: " __VA_ARGS__); \
        syslog_send(_b);                                   \
    } while (0)

/* Троттлинг повторяющихся ошибок шины: не чаще одного сообщения в 5 c,
 * подавленные — счётчиком ("(+N suppressed)"). */
static uint64_t g_err_last_ms = 0;
static unsigned g_err_suppressed = 0;
static uint64_t now_ms(void);

static void err_log(const char *msg)
{
    uint64_t now = now_ms();
    if (g_err_last_ms == 0 || now - g_err_last_ms >= 5000) {
        if (g_err_suppressed)
            GW_LOG("%s (+%u suppressed)\n", msg, g_err_suppressed);
        else
            GW_LOG("%s\n", msg);
        g_err_last_ms = now;
        g_err_suppressed = 0;
    } else {
        g_err_suppressed++;
    }
}

/* ---------------- время / сигналы ------------------------------------------ */
static volatile sig_atomic_t g_stop = 0;
static void on_signal(int sig) { (void)sig; g_stop = 1; }

static uint64_t now_ms(void)
{
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (uint64_t)ts.tv_sec * 1000ull + (uint64_t)ts.tv_nsec / 1000000ull;
}

/* ---------------- Modbus CRC16 --------------------------------------------- */
static uint16_t crc16(const uint8_t *d, int n)
{
    uint16_t crc = 0xFFFF;
    for (int i = 0; i < n; i++) {
        crc ^= d[i];
        for (int b = 0; b < 8; b++) {
            if (crc & 1) crc = (uint16_t)((crc >> 1) ^ 0xA001);
            else         crc = (uint16_t)(crc >> 1);
        }
    }
    return crc;
}

/* ---------------- COM-порт (Modbus RTU): автопоиск своего устройства -------- */
static int      g_serial_fd = -1;
static speed_t  g_speed = B115200;
static int      g_baud = 115200;             /* запрошенный baud (для логов; g_speed — termios-константа) */
static int      g_probe_unit = 1;            /* Modbus-адрес МАП для опроса при поиске */
static long     g_sn = -1;                   /* --sn: ожидаемый серийник МАП (16 бит), -1 = не задан */
static int      g_letter = -1;               /* --sn-letter: ожидаемая буква серийника, -1 = не задана */
static char     g_override_dev[DEVPATH_MAX]; /* -d: устройство задано явно */
static int      g_have_override = 0;
static char     g_found_dev[DEVPATH_MAX];    /* устройство, найденное автоопределением */
static uint64_t g_serial_retry_at = 0;
static int      g_fail_streak = 0;

static const char *dev_used(void)
{
    if (g_have_override) return g_override_dev;
    return g_found_dev[0] ? g_found_dev : NULL;
}

static void port_configure(int fd)
{
    struct termios t;
    if (tcgetattr(fd, &t) != 0) memset(&t, 0, sizeof t);
    t.c_iflag &= (tcflag_t)~(IGNBRK | BRKINT | PARMRK | ISTRIP | INLCR | IGNCR | ICRNL | IXON);
    t.c_oflag &= (tcflag_t)~OPOST;
    t.c_lflag &= (tcflag_t)~(ECHO | ECHONL | ICANON | ISIG | IEXTEN);
    t.c_cflag &= (tcflag_t)~(CSIZE | PARENB | CSTOPB | CRTSCTS);
    t.c_cflag |= (CS8 | CLOCAL | CREAD);
    cfsetispeed(&t, g_speed);
    cfsetospeed(&t, g_speed);
    t.c_cc[VMIN]  = 0;
    t.c_cc[VTIME] = 0;
    tcsetattr(fd, TCSANOW, &t);
}

static int serial_open_path(const char *dev)
{
    int fd = open(dev, O_RDWR | O_NOCTTY | O_NONBLOCK);
    if (fd < 0) {
        GW_LOG("open %s: %s\n", dev, strerror(errno));
        return -1;
    }
    port_configure(fd);
    tcflush(fd, TCIOFLUSH);
    return fd;
}

/* Занят ли порт другим процессом (по /proc/PID/fd) — как в bmslistener. */
static int port_in_use_by_other(const char *dev)
{
    struct stat st_dev, st_fd;
    if (stat(dev, &st_dev) != 0) return 1;
    DIR *dp = opendir("/proc");
    if (!dp) return 1;
    struct dirent *ent;
    int in_use = 0;
    char pd[64], fdpath[256], target[256];
    while ((ent = readdir(dp)) && !in_use) {
        if (ent->d_name[0] < '0' || ent->d_name[0] > '9') continue;
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
            if (stat(target, &st_fd) == 0 && S_ISCHR(st_fd.st_mode) &&
                st_fd.st_rdev == st_dev.st_rdev) { in_use = 1; break; }
        }
        closedir(fdd);
    }
    closedir(dp);
    return in_use;
}

/*
 * Короткий Modbus-опрос идентификации: это наш МАП?
 * Читаем блок ячеек 0x00..0x2F (24 регистра = 48 байт) и проверяем:
 *   - валидный кадр 0x03 (unit/CRC);
 *   - сигнатуру семейства МАП: _VerPO(0x02)!=0 и _DevOpt(0x07) in {1,2,3};
 *   - при заданном --sn: серийник _SerialNum0/1 (0x18 мл., 0x19 ст.) == ожидаемому;
 *   - при заданном --sn-letter: _SerialNum3(0x23) == букве.
 */
static int dev_is_map(const char *dev)
{
    int fd = serial_open_path(dev);
    if (fd < 0) return 0;

    uint8_t req[8];
    req[0] = (uint8_t)g_probe_unit;
    req[1] = 0x03;
    req[2] = 0x00; req[3] = 0x00;   /* start = 0x0000 */
    req[4] = 0x00; req[5] = 0x18;   /* count = 24 регистра (48 байт: 0x00..0x2F) */
    uint16_t c = crc16(req, 6);
    req[6] = (uint8_t)(c & 0xff);
    req[7] = (uint8_t)(c >> 8);

    tcflush(fd, TCIFLUSH);
    if (write(fd, req, sizeof req) != (ssize_t)sizeof req) { close(fd); return 0; }

    uint8_t buf[128];
    int got = 0;
    uint64_t dl = now_ms() + PROBE_MS;
    while (now_ms() < dl && got < 3) {
        struct pollfd p = { fd, POLLIN, 0 };
        int pr = poll(&p, 1, 50);
        if (pr > 0) {
            ssize_t r = read(fd, buf + got, sizeof buf - (size_t)got);
            if (r > 0) got += (int)r;
        }
    }
    if (got >= 3) {
        uint8_t fn = buf[1];
        int need = (fn & 0x80) ? 5 : 3 + buf[2] + 2;
        if (need > (int)sizeof buf) { close(fd); return 0; }
        while (now_ms() < dl && got < need) {
            struct pollfd p = { fd, POLLIN, 0 };
            int pr = poll(&p, 1, 50);
            if (pr > 0) {
                ssize_t r = read(fd, buf + got, sizeof buf - (size_t)got);
                if (r > 0) got += (int)r;
            }
        }
        close(fd);
        if (got < need || buf[0] != (uint8_t)g_probe_unit) return 0;
        if (fn != 0x03) return 0;                       /* не МАП или исключение */
        uint16_t w = (uint16_t)(buf[need - 2] | (buf[need - 1] << 8));
        if (crc16(buf, need - 2) != w) return 0;

        const uint8_t *d = buf + 3;                     /* данные: unit+func+bc */
        int dlen = buf[2];
        if (dlen < 0x24) return 0;
        uint8_t verpo = d[0x02];
        uint8_t devopt = d[0x07];
        unsigned sn = (unsigned)d[0x18] | ((unsigned)d[0x19] << 8);
        uint8_t letter = d[0x23];

        if (verpo == 0) return 0;                       /* сигнатура семейства МАП */
        if (devopt != 1 && devopt != 2 && devopt != 3) return 0;
        if (g_sn >= 0 && (long)sn != g_sn) return 0;
        if (g_letter >= 0 && (int)letter != g_letter) return 0;

        GW_LOG("probe %s: sn=%u devopt=%u verPO=0x%02X letter=%u -> MAP\n",
               dev, sn, devopt, verpo, letter);
        return 1;
    }
    close(fd);
    return 0;
}

/* Кандидаты автопоиска: /dev/ttyUSB{0..N}, /dev/ttyACM{0..N} и /dev/serial/by-id (симлинки). */
static int build_scan_candidates(glob_t *g)
{
    memset(g, 0, sizeof *g);
    /* GLOB_NOCHECK: отсутствие совпадений не ошибка, а литерал шаблона —
     * он отсеется проверкой stat при обходе. Иначе первый же пустой шаблон
     * (напр. нет ttyUSB, но есть ttyACM) сорвал бы весь скан. */
    if (glob("/dev/ttyUSB*", GLOB_NOCHECK, NULL, g) != 0) {
        globfree(g);
        return -1;
    }
    glob("/dev/ttyACM*", GLOB_NOCHECK | GLOB_APPEND, NULL, g);
    glob("/dev/serial/by-id/*", GLOB_NOCHECK | GLOB_APPEND, NULL, g);
    return 0;
}

/*
 * Сканируем свободные последовательные порты и ищем наш МАП.
 * За один вызов проверяется не более одного кандидата: так event loop не
 * блокируется на PROBE_MS * (число портов) при каждом сбое шины. Список
 * кандидатов сохраняется между вызовами до исчерпания или находки.
 */
static glob_t g_scan;
static int    g_scan_active = 0;
static size_t g_scan_idx = 0;
static dev_t  g_scan_seen[64];
static int    g_scan_seen_n = 0;

static void scan_reset(void)
{
    if (g_scan_active) { globfree(&g_scan); g_scan_active = 0; }
    g_scan_idx = 0;
    g_scan_seen_n = 0;
}

/* Один и тот же порт может встретиться и как ttyUSB*, и как by-id — дедуп. */
static int scan_seen(dev_t rdev)
{
    for (int i = 0; i < g_scan_seen_n; i++)
        if (g_scan_seen[i] == rdev) return 1;
    if (g_scan_seen_n < (int)(sizeof(g_scan_seen) / sizeof(g_scan_seen[0])))
        g_scan_seen[g_scan_seen_n++] = rdev;
    return 0;
}

static int discover_device(char *out, size_t outn)
{
    if (!g_scan_active) {
        if (build_scan_candidates(&g_scan) != 0)
            return 0;
        g_scan_active = 1;
        g_scan_idx = 0;
        g_scan_seen_n = 0;
    }

    if (g_scan_idx >= g_scan.gl_pathc) {   /* список исчерпан — начнём заново */
        scan_reset();
        return 0;
    }

    const char *dev = g_scan.gl_pathv[g_scan_idx++];

    struct stat st;
    if (stat(dev, &st) != 0 || !S_ISCHR(st.st_mode))
        return 0;                          /* пропал или не tty — следующий */
    if (scan_seen(st.st_rdev))
        return 0;                          /* уже проверяли в этом проходе */
    if (port_in_use_by_other(dev))
        return 0;                          /* порт занят (bmslistener/mapd/…) */

    if (dev_is_map(dev)) {
        snprintf(out, outn, "%s", dev);
        GW_LOG("scan: МАП найден на %s\n", dev);
        scan_reset();
        return 1;
    }
    return 0;   /* проверен один кандидат — вернём управление event loop */
}

/* Гарантирует открытый порт: берёт заданный/-найденный; иначе ищет среди свободных. */
static int serial_ensure(void)
{
    if (g_serial_fd >= 0)
        return g_serial_fd;

    uint64_t t = now_ms();
    if (t < g_serial_retry_at)
        return -1;
    g_serial_retry_at = t + 2000;   /* не чаще, чем раз в 2 c */

    const char *dev = dev_used();
    if (!dev) {
        if (!discover_device(g_found_dev, sizeof g_found_dev))
            return -1;
        dev = g_found_dev;
    }

    int fd = serial_open_path(dev);
    if (fd < 0) {
        if (!g_have_override)
            g_found_dev[0] = 0;     /* устройство пропало — искать заново */
        return -1;
    }
    g_serial_fd = fd;
    g_fail_streak = 0;
    GW_LOG("opened %s at %d baud\n", dev, g_baud);
    return g_serial_fd;
}

static void serial_drop(const char *why)
{
    if (g_serial_fd >= 0) {
        close(g_serial_fd);
        g_serial_fd = -1;
        GW_LOG("serial closed (%s)\n", why);
    }
}

/*
 * Транзакция RTU: пишем req (unit+pdu+crc), ждём ответ. Возвращает 0 и длину
 * ответа в *rlen_out, либо -1 (таймаут/ошибка/плохой CRC).
 */
static int serial_transaction(const uint8_t *req, int reqlen,
                              uint8_t *resp, int *rlen_out, int timeout_ms)
{
    if (serial_ensure() < 0)
        return -1;

    tcflush(g_serial_fd, TCIFLUSH);

    uint64_t deadline = now_ms() + (uint64_t)timeout_ms;

    int off = 0;
    while (off < reqlen) {
        struct pollfd p = { g_serial_fd, POLLOUT, 0 };
        int pr = poll(&p, 1, 200);
        if (pr < 0) { if (errno == EINTR) continue; serial_drop("poll out"); return -1; }
        if (pr == 0) { if (now_ms() > deadline) return -1; continue; }
        ssize_t w = write(g_serial_fd, req + off, (size_t)(reqlen - off));
        if (w < 0) {
            if (errno == EAGAIN || errno == EINTR) continue;
            serial_drop("write");
            return -1;
        }
        off += (int)w;
        if (now_ms() > deadline) return -1;
    }

    int got = 0, need = 0;
    while (now_ms() < deadline) {
        struct pollfd p = { g_serial_fd, POLLIN, 0 };
        int left = (int)(deadline - now_ms());
        int pr = poll(&p, 1, left > 0 ? left : 1);
        if (pr < 0) { if (errno == EINTR) continue; serial_drop("poll in"); return -1; }
        if (pr == 0) break;
        ssize_t r = read(g_serial_fd, resp + got, (size_t)(RTU_MAX - got));
        if (r < 0) {
            if (errno == EAGAIN || errno == EINTR) continue;
            serial_drop("read");
            return -1;
        }
        if (r == 0) continue;
        got += (int)r;
        if (need == 0 && got >= 3) {
            if (resp[1] & 0x80) need = 5;                 /* exception: unit/func/exc/crc */
            else                need = 3 + resp[2] + 2;   /* unit/func/bc/data/crc */
        }
        if (need && got >= need) break;
    }

    if (g_verbose) {
        GW_LOG("tx[%d]: %02x %02x %02x %02x %02x %02x\n", reqlen,
               req[0], req[1], req[2], req[3], req[4], reqlen > 5 ? req[5] : 0);
        GW_LOG("rx[%d] need=%d: %02x %02x %02x %02x %02x\n", got, need,
               got > 0 ? resp[0] : 0, got > 1 ? resp[1] : 0,
               got > 2 ? resp[2] : 0, got > 3 ? resp[3] : 0, got > 4 ? resp[4] : 0);
    }

    if (need == 0 || got < need) {
        char b[128];
        snprintf(b, sizeof(b), "serial timeout: got=%d need=%d", got, need);
        err_log(b);
        if (++g_fail_streak >= 10 && !g_have_override) {
            g_found_dev[0] = 0;          /* слишком много сбоев — искать устройство заново */
            serial_drop("repeated failures");
        }
        return -1;
    }

    uint16_t want = (uint16_t)(resp[need - 2] | (resp[need - 1] << 8));
    if (crc16(resp, need - 2) != want) {
        char b[128];
        snprintf(b, sizeof(b), "serial bad crc: got=%d need=%d calc=%04x got=%02x%02x",
                 got, need, crc16(resp, need - 2), resp[need - 2], resp[need - 1]);
        err_log(b);
        if (++g_fail_streak >= 10 && !g_have_override) {
            g_found_dev[0] = 0;
            serial_drop("repeated failures");
        }
        return -1;
    }

    g_fail_streak = 0;
    *rlen_out = need;
    return 0;
}

/* ---------------- TCP-сервер (Modbus TCP) ---------------------------------- */
struct client {
    int      fd;
    uint8_t  buf[MAX_FRAME * 2];  /* входной поток MBAP */
    int      len;
    uint8_t  out[CLIENT_OUTBUF];  /* недописанный ответ */
    int      out_off;             /* смещение непереданной части в out */
    int      out_len;             /* сколько байт ещё не передано */
    uint64_t last_ms;             /* время последней активности */
    int      dead;               /* сокет сломан — слот освободить */
};

static void client_reset(struct client *c)
{
    if (c->fd >= 0) close(c->fd);
    c->fd = -1;
    c->len = 0;
    c->out_off = 0;
    c->out_len = 0;
    c->dead = 0;
}

/* Дописывает накопленный ответ: EAGAIN — ждём POLLOUT, ошибка — dead. */
static void client_flush(struct client *c)
{
    while (c->fd >= 0 && c->out_len > 0) {
        ssize_t w = write(c->fd, c->out + c->out_off, (size_t)c->out_len);
        if (w > 0) {
            c->out_off += (int)w;
            c->out_len -= (int)w;
            continue;
        }
        if (w < 0 && (errno == EAGAIN || errno == EWOULDBLOCK || errno == EINTR))
            break;                 /* сокет не готов — допишем по POLLOUT */
        c->dead = 1;               /* EPIPE/ECONNRESET/… — клиент отвалился */
        return;
    }
    if (c->out_off > 0) {
        if (c->out_len > 0)
            memmove(c->out, c->out + c->out_off, (size_t)c->out_len);
        c->out_off = 0;
    }
}

/* Ставит ответ в очередь и пытается отдать его, не блокируя event loop. */
static void send_mbap(struct client *c, uint16_t txn, uint8_t unit,
                      const uint8_t *pdu, int pdu_len)
{
    if (c->fd < 0 || c->dead) return;
    if (pdu_len < 0 || pdu_len > MAX_FRAME - 7) { c->dead = 1; return; }

    uint8_t out[MAX_FRAME];
    int total = 7 + pdu_len;
    out[0] = (uint8_t)(txn >> 8); out[1] = (uint8_t)(txn & 0xff);
    out[2] = 0; out[3] = 0;
    out[4] = (uint8_t)((pdu_len + 1) >> 8);
    out[5] = (uint8_t)((pdu_len + 1) & 0xff);
    out[6] = unit;
    memcpy(out + 7, pdu, (size_t)pdu_len);

    if (c->out_len + total > (int)sizeof(c->out)) {
        GW_LOG("client %d send queue overflow, dropping\n", c->fd);
        c->dead = 1;
        return;
    }
    memcpy(c->out + c->out_len, out, (size_t)total);
    c->out_len += total;
    client_flush(c);
}

static void send_exception(struct client *c, uint16_t txn, uint8_t unit,
                           uint8_t func, uint8_t code)
{
    uint8_t pdu[2] = { (uint8_t)(func | 0x80), code };
    send_mbap(c, txn, unit, pdu, 2);
}

/* Обработка одного MBAP-запроса целиком (frame длиной framelen). */
static void handle_frame(struct client *c, const uint8_t *frame, int framelen)
{
    if (framelen < 8) return;
    uint16_t txn  = (uint16_t)((frame[0] << 8) | frame[1]);
    uint16_t proto = (uint16_t)((frame[2] << 8) | frame[3]);
    uint16_t mlen  = (uint16_t)((frame[4] << 8) | frame[5]);
    uint8_t  unit  = frame[6];
    int      plen  = mlen - 1;                 /* длина PDU */
    if (proto != 0 || plen < 1 || 7 + plen > framelen) return;
    const uint8_t *pdu = frame + 7;

    /* RTU: unit + PDU + CRC(lo,hi) */
    uint8_t rtu[RTU_MAX];
    if (plen + 3 > RTU_MAX) { send_exception(c, txn, unit, pdu[0], 0x03); return; }
    rtu[0] = unit;
    memcpy(rtu + 1, pdu, (size_t)plen);
    uint16_t c16 = crc16(rtu, plen + 1);
    rtu[plen + 1] = (uint8_t)(c16 & 0xff);
    rtu[plen + 2] = (uint8_t)(c16 >> 8);

    uint8_t resp[RTU_MAX];
    int rlen = 0;
    if (serial_transaction(rtu, plen + 3, resp, &rlen, 1000) != 0) {
        send_exception(c, txn, unit, pdu[0], 0x0B);   /* gateway target failed */
        return;
    }
    if (rlen < 3 || resp[0] != unit) {
        send_exception(c, txn, unit, pdu[0], 0x0B);
        return;
    }
    /* resp: unit + PDU(без CRC) — отдаём в MBAP */
    send_mbap(c, txn, resp[0], resp + 1, rlen - 3);
}

static speed_t parse_baud(int b)
{
    switch (b) {
        case 9600:   return B9600;
        case 19200:  return B19200;
        case 38400:  return B38400;
        case 57600:  return B57600;
        case 115200: return B115200;
        default:     return B115200;
    }
}

static void usage(const char *a)
{
    fprintf(stderr,
        "usage: %s [-d device] [-b baud] [-u unit] [--sn N] [--sn-letter C]\n"
        "          [-p tcp_port] [-l listen_addr] [-v]\n"
        "  device: по умолчанию автопоиск (свободные /dev/ttyUSB*, /dev/ttyACM*,\n"
        "          /dev/serial/by-id/*; проверка идентификации МАП: _VerPO!=0,\n"
        "          _DevOpt in {1,2,3}, при --sn — совпадение серийника 0x18/0x19)\n"
        "  --sn CHANGE_ME — заглушка: фильтр по серийнику отключается (не задаёт sn=0)\n"
        "  baud=115200, unit=1, tcp_port=%d, listen=%s\n",
        a, DEF_TCP_PORT, DEF_LISTEN);
}

/* Строгий разбор целого аргумента: вся строка, диапазон [min,max].
 * base=10 — без сюрприза восьмеричных ведущих нулей; base=0 — префиксы 0x/0. */
static int parse_int_arg(const char *s, int base, long min, long max, long *out)
{
    errno = 0;
    char *end = NULL;
    long v = strtol(s, &end, base);
    if (errno != 0 || end == s || *end != '\0' || v < min || v > max)
        return -1;
    *out = v;
    return 0;
}

int main(int argc, char **argv)
{
    const char *listen_addr = DEF_LISTEN;
    int tcp_port = DEF_TCP_PORT;
    int baud = 115200;

    for (int i = 1; i < argc; i++) {
        if (!strcmp(argv[i], "--version") || !strcmp(argv[i], "-V")) {
            printf("mapgateway %s\n", VERSION);
            return 0;
        } else if (!strcmp(argv[i], "-d") && i + 1 < argc) {
            snprintf(g_override_dev, sizeof(g_override_dev), "%s", argv[++i]);
            g_have_override = 1;
        } else if (!strcmp(argv[i], "-u") && i + 1 < argc) {
            long v;
            if (parse_int_arg(argv[++i], 10, 1, 247, &v) != 0) {
                fprintf(stderr, "mapgateway: некорректный -u '%s' (1..247)\n", argv[i]);
                return 2;
            }
            g_probe_unit = (int)v;
        } else if (!strcmp(argv[i], "--sn") && i + 1 < argc) {
            const char *v = argv[++i];
            if (!strcasecmp(v, "CHANGE_ME")) {
                g_sn = -1;
                fprintf(stderr, "mapgateway: --sn CHANGE_ME -> фильтр по серийнику отключён\n");
            } else {
                long x;
                /* base 0: допускаются 0x-HEX и десятичные; диапазон 0..65535. */
                if (parse_int_arg(v, 0, 0, 0xFFFF, &x) != 0) {
                    fprintf(stderr, "mapgateway: некорректный --sn '%s' (0..65535)\n", v);
                    return 2;
                }
                g_sn = x;
            }
        } else if (!strcmp(argv[i], "--sn-letter") && i + 1 < argc) {
            const char *v = argv[++i];
            if (v[0] == '\0') {
                fprintf(stderr, "mapgateway: пустой --sn-letter\n");
                return 2;
            }
            g_letter = (unsigned char)v[0];
        } else if (!strcmp(argv[i], "-b") && i + 1 < argc) {
            long v;
            if (parse_int_arg(argv[++i], 10, 0, 1000000, &v) != 0 ||
                (v != 9600 && v != 19200 && v != 38400 && v != 57600 && v != 115200)) {
                fprintf(stderr, "mapgateway: некорректный -b '%s' (9600|19200|38400|57600|115200)\n", argv[i]);
                return 2;
            }
            baud = (int)v;
        } else if (!strcmp(argv[i], "-p") && i + 1 < argc) {
            long v;
            if (parse_int_arg(argv[++i], 10, 1, 65535, &v) != 0) {
                fprintf(stderr, "mapgateway: некорректный -p '%s' (1..65535)\n", argv[i]);
                return 2;
            }
            tcp_port = (int)v;
        } else if (!strcmp(argv[i], "-l") && i + 1 < argc) {
            listen_addr = argv[++i];
        } else if (!strcmp(argv[i], "-v")) {
            g_verbose = 1;
        } else if (!strcmp(argv[i], "-h") || !strcmp(argv[i], "--help")) {
            usage(argv[0]);
            return 0;
        } else {
            usage(argv[0]);
            return 2;
        }
    }

    g_speed = parse_baud(baud);
    g_baud = baud;

    signal(SIGPIPE, SIG_IGN);
    signal(SIGTERM, on_signal);
    signal(SIGINT,  on_signal);

    int lfd = socket(AF_INET, SOCK_STREAM, 0);
    if (lfd < 0) { GW_LOG("socket: %s\n", strerror(errno)); return 1; }
    int one = 1;
    setsockopt(lfd, SOL_SOCKET, SO_REUSEADDR, &one, sizeof(one));

    struct sockaddr_in sa;
    memset(&sa, 0, sizeof(sa));
    sa.sin_family = AF_INET;
    sa.sin_port = htons((uint16_t)tcp_port);
    if (inet_pton(AF_INET, listen_addr, &sa.sin_addr) != 1) {
        GW_LOG("bad listen addr %s\n", listen_addr);
        return 1;
    }
    if (bind(lfd, (struct sockaddr *)&sa, sizeof(sa)) != 0) {
        GW_LOG("bind %s:%d: %s\n", listen_addr, tcp_port, strerror(errno));
        return 1;
    }
    if (listen(lfd, 8) != 0) { GW_LOG("listen: %s\n", strerror(errno)); return 1; }

    GW_LOG("started: dev=%s baud=%d unit=%d sn=%ld listen=%s:%d\n",
           g_have_override ? g_override_dev : "auto", baud, g_probe_unit, g_sn, listen_addr, tcp_port);
    serial_ensure();   /* пробуем открыть сразу (не критично) */

    struct client cl[MAX_CLIENTS];
    memset(cl, 0, sizeof(cl));
    for (int i = 0; i < MAX_CLIENTS; i++) cl[i].fd = -1;

    while (!g_stop) {
        uint64_t now = now_ms();

        /* Слоты «мёртвых» и простаивающих клиентов не должны выедать MAX_CLIENTS. */
        for (int i = 0; i < MAX_CLIENTS; i++) {
            if (cl[i].fd < 0) continue;
            if (cl[i].dead) {
                GW_LOG("client slot %d dropped\n", i);
                client_reset(&cl[i]);
                continue;
            }
            /* Любой клиент без активности >= CLIENT_IDLE_MS закрывается, в т.ч.
               «живой, но нечитающий» с непустой исходящей очередью: иначе после
               заполнения send-буфера он навсегда удерживал бы слот (out_len > 0
               исключал его из рипера). Нормальный запрос обрабатывается за мс. */
            if (now - cl[i].last_ms >= CLIENT_IDLE_MS) {
                GW_LOG("client slot %d idle timeout, closing\n", i);
                client_reset(&cl[i]);
            }
        }

        struct pollfd pfds[1 + MAX_CLIENTS];
        int      map[1 + MAX_CLIENTS];
        int n = 0;
        pfds[n].fd = lfd; pfds[n].events = POLLIN; map[n] = -1; n++;
        for (int i = 0; i < MAX_CLIENTS; i++) {
            if (cl[i].fd >= 0) {
                pfds[n].fd = cl[i].fd;
                pfds[n].events = (short)(POLLIN | (cl[i].out_len > 0 ? POLLOUT : 0));
                map[n] = i; n++;
            }
        }
        int pr = poll(pfds, (nfds_t)n, 500);
        if (pr < 0) { if (errno == EINTR) continue; GW_LOG("poll: %s\n", strerror(errno)); break; }
        if (pr == 0) continue;

        if (pfds[0].revents & POLLIN) {
            struct sockaddr_in ca;
            socklen_t cl_len = sizeof(ca);
            int cfd = accept(lfd, (struct sockaddr *)&ca, &cl_len);
            if (cfd >= 0) {
                fcntl(cfd, F_SETFL, fcntl(cfd, F_GETFL, 0) | O_NONBLOCK);
                setsockopt(cfd, IPPROTO_TCP, TCP_NODELAY, &one, sizeof(one));
                setsockopt(cfd, SOL_SOCKET, SO_KEEPALIVE, &one, sizeof(one));
#ifdef TCP_KEEPIDLE
                int ka = 60;
                setsockopt(cfd, IPPROTO_TCP, TCP_KEEPIDLE, &ka, sizeof(ka));
                ka = 10;
                setsockopt(cfd, IPPROTO_TCP, TCP_KEEPINTVL, &ka, sizeof(ka));
                ka = 3;
                setsockopt(cfd, IPPROTO_TCP, TCP_KEEPCNT, &ka, sizeof(ka));
#endif
                int slot = -1;
                for (int i = 0; i < MAX_CLIENTS; i++) if (cl[i].fd < 0) { slot = i; break; }
                if (slot < 0) {
                    GW_LOG("too many clients, rejecting\n");
                    close(cfd);
                } else {
                    cl[slot].fd = cfd;
                    cl[slot].len = 0;
                    cl[slot].out_off = 0;
                    cl[slot].out_len = 0;
                    cl[slot].dead = 0;
                    cl[slot].last_ms = now_ms();
                    GW_LOG("client %s:%d connected\n",
                           inet_ntoa(ca.sin_addr), ntohs(ca.sin_port));
                }
            }
        }

        for (int k = 1; k < n; k++) {
            int ci = map[k];
            struct client *c = &cl[ci];
            if (c->fd < 0) continue;

            if (pfds[k].revents & POLLOUT)
                client_flush(c);
            if (c->dead) { client_reset(c); continue; }

            if (!(pfds[k].revents & (POLLIN | POLLHUP | POLLERR)))
                continue;

            /* Неблокирующее чтение всего доступного: сокет помечен O_NONBLOCK. */
            for (;;) {
                if (c->len >= (int)sizeof(c->buf)) break;
                ssize_t r = read(c->fd, c->buf + c->len,
                                 sizeof(c->buf) - (size_t)c->len);
                if (r > 0) {
                    c->len += (int)r;
                    c->last_ms = now_ms();
                    continue;
                }
                if (r == 0) { c->dead = 1; break; }
                if (errno == EINTR) continue;
                if (errno == EAGAIN || errno == EWOULDBLOCK) break;
                c->dead = 1; break;
            }
            if (c->dead) {
                GW_LOG("client slot %d disconnected\n", ci);
                client_reset(c);
                continue;
            }

            /* обрабатываем все полные MBAP-кадры в буфере */
            int off = 0;
            while (c->len - off >= 6) {
                uint16_t mlen = (uint16_t)((c->buf[off + 4] << 8) | c->buf[off + 5]);
                int total = 6 + mlen;
                if (total < 8 || total > (int)sizeof(c->buf)) { off = c->len; break; }
                if (c->len - off < total) break;
                handle_frame(c, c->buf + off, total);
                if (c->dead) break;    /* send_mbap мог пометить сокет мёртвым */
                off += total;
            }
            if (off > 0) {
                memmove(c->buf, c->buf + off, (size_t)(c->len - off));
                c->len -= off;
            }
            if (c->dead) {
                GW_LOG("client slot %d dropped on send\n", ci);
                client_reset(c);
                continue;
            }
            if (c->len >= (int)sizeof(c->buf)) {
                GW_LOG("client slot %d buffer overflow, dropping\n", ci);
                client_reset(c);
            }
        }
    }

    GW_LOG("stopping\n");
    for (int i = 0; i < MAX_CLIENTS; i++) if (cl[i].fd >= 0) close(cl[i].fd);
    close(lfd);
    serial_drop("exit");
    scan_reset();        /* освободить незавершённый скан (glob) на выходе */
    return 0;
}
