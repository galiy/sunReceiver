#!/usr/bin/env python2
# -*- coding: utf-8 -*-
"""
ANT BMS 22PHB-TB-8-22S-240A — тестовый клиент считывания телеметрии по UART.
Работает на Python 2 (ПАК «Малина», без pyserial — читаем/пишем через termios).

Назначение: активно будить BMS и читать/разбирать 140-байтные live-кадры
(заголовок AA 55 AA FF), классический протокол ANT (5A5A read).

Подключение (пин 5 жёлтый = TXD BMS / выход, пин 6 зелёный = RXD BMS / вход):
  BMS (жёлтый, пин5/TXD)  -> RXD адаптера (белый)          # BMS передаёт данные
  BMS (зелёный, пин6/RXD) <- TXD адаптера (зелёный)        # адаптер шлёт команды
  BMS (чёрный, пин4/GND)  <- GND адаптера (чёрный)
  красный (+5В) не подключается.

Скорость протокола — 19200 бод 8N1 (проверено подбором).

Логика: раз в ~250 мс в BMS уходит keep-alive запрос телеметрии
  5A 5A 00 00 01 01
(read addr=0 data=0x0001, checksum=0x00+0x00+0x01=0x01). В ответ BMS начинает
вещать 140-байтные кадры AA 55 AA FF ... — каждые 140 байт. Кадры валидируются
по контрольной сумме (сумма байтов offset 4..137 == checksum BE на offset 138).

Запуск:
  python2 antbms_poll.py [порт] [скорость] [длительность_с]
  пример: python2 antbms_poll.py /dev/ttyUSB1 19200 60
"""

import os, sys, time, signal, threading, select

# ---------------------------------------------------------------------------
# Параметры по умолчанию
# ---------------------------------------------------------------------------
PORT = sys.argv[1] if len(sys.argv) > 1 else "/dev/ttyUSB1"
BAUD = int(sys.argv[2]) if len(sys.argv) > 2 else 19200
SECS = int(sys.argv[3]) if len(sys.argv) > 3 else 60

KEEPALIVE = '\x5a\x5a\x00\x00\x01\x01'      # request телеметрии
KEEPALIVE_PERIOD = 0.25                     # с
HEADER = '\xaa\x55\xaa\xff'
FRAME_LEN = 140

FSM_CMD = {
    0: "Off", 1: "Open", 2: "Overvoltage protection",
    3: "Over current protection", 4: "Battery full", 5: "Total overpressure",
    6: "Battery over temperature", 7: "Power over temperature",
    8: "Abnormal current", 9: "Balanced line dropped string",
    10: "Motherboard over temperature", 11: "Charge on",
    12: "Short circuit protection", 13: "Discharge tube abnormality",
    14: "Start exception", 15: "Manually closed",
}
DSM_CMD = {
    0: "Off", 1: "Open", 2: "Over-discharge protection",
    3: "Over current protection", 5: "Total pressure undervoltage",
    6: "Battery over temperature", 7: "Power over temperature",
    8: "Abnormal current", 9: "Balanced line dropped string",
    10: "Motherboard over temperature", 11: "Charge on",
    12: "Short circuit protection", 13: "Discharge tube abnormality",
    14: "Start exception", 15: "Manually closed",
}
BAL_CMD = {
    0: "Off", 1: "Exceeds the limit equilibrium",
    2: "Charge differential pressure balance", 3: "Balanced over temperature",
    4: "Automatic equalization", 10: "Motherboard over temperature",
}


# ---------------------------------------------------------------------------
# Сериал через termios (Linux/Python 2) — без pyserial
# ---------------------------------------------------------------------------
import termios as T

SPEEDS = {
    2400: T.B2400, 4800: T.B4800, 9600: T.B9600,
    19200: T.B19200, 38400: T.B38400, 115200: T.B115200,
}


def open_serial(port, baud, vmin=1, vtime=2):
    fd = os.open(port, os.O_RDWR | os.O_NOCTTY | os.O_NONBLOCK)
    try:
        t = list(T.tcgetattr(fd))
    except Exception as e:
        raise SystemExit("tcgetattr failed: %s" % e)
    b = SPEEDS.get(baud)
    if b is None:
        raise SystemExit("Unsupported baud %d" % baud)
    c_cflag = t[2]
    c_cflag &= ~(T.CBAUD | T.CSIZE | T.PARENB | T.CSTOPB)
    c_cflag |= b | T.CS8 | T.CREAD | T.CLOCAL
    # t на Linux (py2): [iflag, oflag, cflag, lflag, ispeed, ospeed, cc]
    new = [0, 0, c_cflag, 0, b, b, t[6]]
    new[6][0] = vmin   # VMIN в cc[0]
    new[6][1] = vtime  # VTIME в cc[1]
    T.tcsetattr(fd, T.TCSANOW, new)
    return fd


def serial_read(fd):
    try:
        r, _, _ = select.select([fd], [], [], 0.05)
        if fd in r:
            return os.read(fd, 256)
    except (OSError, IOError):
        pass
    return ""


def serial_write(fd, data):
    try:
        return os.write(fd, data)
    except (OSError, IOError) as e:
        print("write error: %s" % e)
        return 0


def close_serial(fd):
    os.close(fd)


# ---------------------------------------------------------------------------
# Парсинг 140-байтного live-кадра
# ---------------------------------------------------------------------------
def u16(fr, off):
    return (ord(fr[off]) << 8) | ord(fr[off + 1])


def i32(fr, off):
    v = (ord(fr[off]) << 24) | (ord(fr[off+1]) << 16) | \
        (ord(fr[off+2]) << 8) | ord(fr[off+3])
    if v & 0x80000000:
        v -= 0x100000000
    return v


def u32(fr, off):
    return (ord(fr[off]) << 24) | (ord(fr[off+1]) << 16) | \
           (ord(fr[off+2]) << 8) | ord(fr[off+3])


def parse_frame(fr):
    out = {}
    out["header_ok"] = fr[0:4] == HEADER
    cells = []
    for i in range(16):
        cells.append(u16(fr, 6 + i * 2))
    out["cells_mv"] = cells

    out["current_a"] = i32(fr, 70) * 0.1
    out["soc_pct"] = ord(fr[74])
    out["cap_total_ah"] = u32(fr, 75) * 1e-6
    out["cap_remain_ah"] = u32(fr, 79) * 1e-6
    out["cap_cycle_ah"] = u32(fr, 83) * 1e-3
    out["uptime_s"] = u32(fr, 87)

    temps = []
    for i in range(6):
        v = u16(fr, 91 + i * 2)
        if v & 0x8000:
            v -= 0x10000
        temps.append(v)
    out["temp_c"] = temps

    out["charge_mos"] = ord(fr[103])
    out["discharge_mos"] = ord(fr[104])
    out["balance"] = ord(fr[105])
    out["tire_mm"] = u16(fr, 106)
    out["pulses"] = u16(fr, 108)
    out["relay"] = ord(fr[110])
    out["power_w"] = i32(fr, 111)
    out["max_cell_idx"] = ord(fr[115])
    out["max_cell_mv"] = u16(fr, 116)
    out["min_cell_idx"] = ord(fr[118])
    out["min_cell_mv"] = u16(fr, 119)
    out["avg_mv"] = u16(fr, 121)
    out["cell_count"] = ord(fr[123])
    out["bal_bitmask"] = u32(fr, 132)
    out["syslog"] = u16(fr, 136)

    expected = sum(ord(fr[i]) for i in range(4, 138)) & 0xFFFF
    framed = (ord(fr[138]) << 8) | ord(fr[139])
    out["checksum_ok"] = (expected == framed)
    out["checksum_expected"] = expected
    out["checksum_frame"] = framed
    return out


def fmt_cmd(code, table):
    return table.get(code, "(см. таблицу)")


def render(p):
    ok = "OK" if p["checksum_ok"] else "BAD-CRC (exp=0x%04X frame=0x%04X)" % (
        p["checksum_expected"], p["checksum_frame"])
    L = []
    L.append("### frame %s  header_ok=%s  cell_count=%d" % (
        ok, p["header_ok"], p["cell_count"]))
    L.append("  cells[16]: %s" % " ".join(str(v) for v in p["cells_mv"]))
    L.append("  I=%.1f A  SOC=%d%%  P=%d W  Uptime=%ds" % (
        p["current_a"], p["soc_pct"], p["power_w"], p["uptime_s"]))
    L.append("  Cap total=%.2f Ah remain=%.2f Ah cycle=%.3f Ah" % (
        p["cap_total_ah"], p["cap_remain_ah"], p["cap_cycle_ah"]))
    L.append("  Temps: %s" % " ".join(str(t) for t in p["temp_c"]))
    L.append("  ChMOS=%d(%s)  DisMOS=%d(%s)  Bal=%d(%s)" % (
        p["charge_mos"], fmt_cmd(p["charge_mos"], FSM_CMD),
        p["discharge_mos"], fmt_cmd(p["discharge_mos"], DSM_CMD),
        p["balance"], fmt_cmd(p["balance"], BAL_CMD)))
    L.append("  max cell #%d = %d mV; min cell #%d = %d mV; avg = %d mV" % (
        p["max_cell_idx"], p["max_cell_mv"],
        p["min_cell_idx"], p["min_cell_mv"], p["avg_mv"]))
    L.append("  relay=%d  tires=%dmm pulses=%d" % (
        p["relay"], p["tire_mm"], p["pulses"]))
    L.append("  balance_mask=0x%08X  syslog=0x%04X" % (
        p["bal_bitmask"], p["syslog"]))
    return "\n".join(L)


# ---------------------------------------------------------------------------
# Фон: шлём keep-alive, пока не остановим
# ---------------------------------------------------------------------------
def keepalive_loop(fd, stop):
    while not stop.is_set():
        serial_write(fd, KEEPALIVE)
        stop.wait(KEEPALIVE_PERIOD)


def main():
    stop = threading.Event()

    def sighandler(sig, frm):
        stop.set()
    signal.signal(signal.SIGINT, sighandler)

    print("Opening %s @ %d 8N1, %ds collect" % (PORT, BAUD, SECS))
    fd = open_serial(PORT, BAUD)

    ka = threading.Thread(target=keepalive_loop, args=(fd, stop))
    ka.daemon = True
    ka.start()

    buf = ""
    frames = 0
    valid = 0
    start = time.time()
    last_report = time.time()

    try:
        while time.time() - start < SECS and not stop.is_set():
            chunk = serial_read(fd)
            if chunk:
                buf += chunk
            while True:
                idx = buf.find(HEADER)
                if idx == -1:
                    if len(buf) > FRAME_LEN:
                        buf = buf[-FRAME_LEN:]
                    break
                if idx > 0:
                    buf = buf[idx:]
                    continue
                if len(buf) >= FRAME_LEN:
                    fr = buf[:FRAME_LEN]
                    buf = buf[FRAME_LEN:]
                    frames += 1
                    p = parse_frame(fr)
                    if p["checksum_ok"]:
                        valid += 1
                    print(render(p))
                    last_report = time.time()
                else:
                    break
            if frames == 0 and time.time() - last_report > 5:
                print("... no frames yet (buffer=%dB, keep-alive sending)" % len(buf))
                last_report = time.time()
    except KeyboardInterrupt:
        pass
    finally:
        stop.set()
        close_serial(fd)

    print("\nDone: %d frames, %d valid CRC, over %.1fs" % (
        frames, valid, time.time() - start))


if __name__ == "__main__":
    main()