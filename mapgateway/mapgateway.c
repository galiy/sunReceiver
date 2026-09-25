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
 * Совместим как замена прямого MАП-гейта 192.168.13.74:502: sunReceiver
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
 *   device — путь к COM (по умолчанию ищется FTDI by-id, иначе /dev/ttyUSB2)
 *   baud   — 9600|19200|38400|57600|115200 (по умолчанию 115200)
 *   tcp_port — по умолчанию 502
 *   --version / -V — печатает "mapgateway <VERSION>" и выходит
 */

#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
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
#define RTU_MAX       260
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
static int      g_probe_unit = 1;            /* Modbus-адрес МАП для опроса при поиске */
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

/* Короткий Modbus-опрос: это наш МАП? Читаем 1 регистр 0x0400 (MODE). */
static int dev_is_map(const char *dev)
{
    int fd = serial_open_path(dev);
    if (fd < 0) return 0;

    uint8_t req[8];
    req[0] = (uint8_t)g_probe_unit;
    req[1] = 0x03;
    req[2] = 0x04; req[3] = 0x00;   /* адрес 0x0400 */
    req[4] = 0x00; req[5] = 0x01;   /* 1 регистр */
    uint16_t c = crc16(req, 6);
    req[6] = (uint8_t)(c & 0xff);
    req[7] = (uint8_t)(c >> 8);

    tcflush(fd, TCIFLUSH);
    if (write(fd, req, sizeof req) != (ssize_t)sizeof req) { close(fd); return 0; }

    uint8_t buf[16];
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
        if (fn != 0x03 && fn != 0x83) return 0;
        uint16_t w = (uint16_t)(buf[need - 2] | (buf[need - 1] << 8));
        if (crc16(buf, need - 2) != w) return 0;
        return 1;
    }
    close(fd);
    return 0;
}

/* Сканируем свободные /dev/ttyUSB* и ищем наш МАП. */
static int discover_device(char *out, size_t outn)
{
    glob_t g;
    if (glob("/dev/ttyUSB*", 0, NULL, &g) != 0) { globfree(&g); return 0; }
    int found = 0;
    for (size_t i = 0; i < g.gl_pathc && !found; i++) {
        const char *dev = g.gl_pathv[i];
        if (port_in_use_by_other(dev)) continue;   /* порт занят (bmslistener/mapd/…) */
        if (dev_is_map(dev)) {
            snprintf(out, outn, "%s", dev);
            GW_LOG("scan: МАП найден на %s\n", dev);
            found = 1;
        }
    }
    globfree(&g);
    return found;
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
    GW_LOG("opened %s at %u baud\n", dev, (unsigned)115200);
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
    int  fd;
    uint8_t buf[MAX_FRAME * 2];
    int  len;
};

static void send_mbap(int fd, uint16_t txn, uint8_t unit,
                      const uint8_t *pdu, int pdu_len)
{
    uint8_t out[MAX_FRAME];
    int len = pdu_len + 1;              /* unit + pdu */
    out[0] = (uint8_t)(txn >> 8); out[1] = (uint8_t)(txn & 0xff);
    out[2] = 0; out[3] = 0;
    out[4] = (uint8_t)(len >> 8); out[5] = (uint8_t)(len & 0xff);
    out[6] = unit;
    memcpy(out + 7, pdu, (size_t)pdu_len);
    ssize_t n = write(fd, out, (size_t)(7 + pdu_len));
    (void)n;
}

static void send_exception(int fd, uint16_t txn, uint8_t unit, uint8_t func, uint8_t code)
{
    uint8_t pdu[2] = { (uint8_t)(func | 0x80), code };
    send_mbap(fd, txn, unit, pdu, 2);
}

/* Обработка одного MBAP-запроса целиком (frame длиной framelen). */
static void handle_frame(int cfd, const uint8_t *frame, int framelen)
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
    if (plen + 3 > RTU_MAX) { send_exception(cfd, txn, unit, pdu[0], 0x03); return; }
    rtu[0] = unit;
    memcpy(rtu + 1, pdu, (size_t)plen);
    uint16_t c = crc16(rtu, plen + 1);
    rtu[plen + 1] = (uint8_t)(c & 0xff);
    rtu[plen + 2] = (uint8_t)(c >> 8);

    uint8_t resp[RTU_MAX];
    int rlen = 0;
    if (serial_transaction(rtu, plen + 3, resp, &rlen, 1000) != 0) {
        send_exception(cfd, txn, unit, pdu[0], 0x0B);   /* gateway target failed */
        return;
    }
    if (rlen < 3 || resp[0] != unit) {
        send_exception(cfd, txn, unit, pdu[0], 0x0B);
        return;
    }
    /* resp: unit + PDU(без CRC) — отдаём в MBAP */
    send_mbap(cfd, txn, resp[0], resp + 1, rlen - 3);
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
        "usage: %s [-d device] [-b baud] [-u unit] [-p tcp_port] [-l listen_addr] [-v]\n"
        "  device: по умолчанию автопоиск (свободные /dev/ttyUSB*, короткий Modbus-опрос)\n"
        "  baud=115200, unit=1, tcp_port=%d, listen=%s\n",
        a, DEF_TCP_PORT, DEF_LISTEN);
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
            g_probe_unit = atoi(argv[++i]);
        } else if (!strcmp(argv[i], "-b") && i + 1 < argc) {
            baud = atoi(argv[++i]);
        } else if (!strcmp(argv[i], "-p") && i + 1 < argc) {
            tcp_port = atoi(argv[++i]);
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

    GW_LOG("started: dev=%s baud=%d unit=%d listen=%s:%d\n",
           g_have_override ? g_override_dev : "auto", baud, g_probe_unit, listen_addr, tcp_port);
    serial_ensure();   /* пробуем открыть сразу (не критично) */

    struct client cl[MAX_CLIENTS];
    memset(cl, 0, sizeof(cl));
    for (int i = 0; i < MAX_CLIENTS; i++) cl[i].fd = -1;

    while (!g_stop) {
        struct pollfd pfds[1 + MAX_CLIENTS];
        int      map[1 + MAX_CLIENTS];
        int n = 0;
        pfds[n].fd = lfd; pfds[n].events = POLLIN; map[n] = -1; n++;
        for (int i = 0; i < MAX_CLIENTS; i++) {
            if (cl[i].fd >= 0) {
                pfds[n].fd = cl[i].fd; pfds[n].events = POLLIN; map[n] = i; n++;
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
                setsockopt(cfd, IPPROTO_TCP, TCP_NODELAY, &one, sizeof(one));
                int slot = -1;
                for (int i = 0; i < MAX_CLIENTS; i++) if (cl[i].fd < 0) { slot = i; break; }
                if (slot < 0) {
                    GW_LOG("too many clients, rejecting\n");
                    close(cfd);
                } else {
                    cl[slot].fd = cfd; cl[slot].len = 0;
                    GW_LOG("client %s:%d connected\n",
                           inet_ntoa(ca.sin_addr), ntohs(ca.sin_port));
                }
            }
        }

        for (int k = 1; k < n; k++) {
            if (!(pfds[k].revents & (POLLIN | POLLHUP | POLLERR))) continue;
            int ci = map[k];
            if (cl[ci].fd < 0) continue;
            ssize_t r = read(cl[ci].fd, cl[ci].buf + cl[ci].len,
                             sizeof(cl[ci].buf) - (size_t)cl[ci].len);
            if (r <= 0) {
                GW_LOG("client slot %d disconnected\n", ci);
                close(cl[ci].fd);
                cl[ci].fd = -1; cl[ci].len = 0;
                continue;
            }
            cl[ci].len += (int)r;

            /* обрабатываем все полные MBAP-кадры в буфере */
            int off = 0;
            while (cl[ci].len - off >= 6) {
                uint16_t mlen = (uint16_t)((cl[ci].buf[off + 4] << 8) | cl[ci].buf[off + 5]);
                int total = 6 + mlen;
                if (total < 8 || total > (int)sizeof(cl[ci].buf)) { off = cl[ci].len; break; }
                if (cl[ci].len - off < total) break;
                handle_frame(cl[ci].fd, cl[ci].buf + off, total);
                off += total;
            }
            if (off > 0) {
                memmove(cl[ci].buf, cl[ci].buf + off, (size_t)(cl[ci].len - off));
                cl[ci].len -= off;
            }
        }
    }

    GW_LOG("stopping\n");
    for (int i = 0; i < MAX_CLIENTS; i++) if (cl[i].fd >= 0) close(cl[i].fd);
    close(lfd);
    serial_drop("exit");
    return 0;
}
