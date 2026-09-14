#!/usr/bin/env python2
# -*- coding: utf-8 -*-
# Мониторинг линии RXD дисплея -> BMS ANT (зелёный провод), линия, на которой
# дисплей шлёт heartbeat/инициализацию в BMS.
# Скорость по умолчанию 9600 (Small Ant дисплей 5V, 9600 8N1).
# Без pyserial: читаем /dev/ttyUSB1 напрямую (os/termios), подходит для Малины (Python 2).
# Выводим каждый пакет (burst между паузами) с монотонным временем и покадровыми таймингами.

import os, sys, time, termios, select, signal, array

PORT   = sys.argv[1] if len(sys.argv) > 1 else "/dev/ttyUSB1"
BAUD   = int(sys.argv[2]) if len(sys.argv) > 2 else 9600
SECS   = int(sys.argv[3]) if len(sys.argv) > 3 else 75
LOG    = sys.argv[4] if len(sys.argv) > 4 else "/tmp/antbms_init_monitor.log"

GAP    = 0.12      # пауза > этой (с) закрывает текущий burst
STALL  = 0.030     # если пауза между байтами заметно > этого - пометить

def set_baud(t, baud):
    # termios (Linux, Python 2): список из 7 элементов
    #   [iflag, oflag, cflag, lflag, ispeed, ospeed, cc]
    # скорость кодируется в cflag и ispeed/ospeed.
    import termios as T
    speeds = {9600: T.B9600, 19200: T.B19200, 2400: T.B2400,
              4800: T.B4800, 115200: T.B115200, 38400: T.B38400}
    b = speeds.get(baud, T.B9600)
    c_cflag = t[2]
    c_cflag &= ~(T.CBAUD | T.CSIZE | T.PARENB | T.CSTOPB)
    c_cflag |= b | T.CS8 | T.CREAD | T.CLOCAL
    return [0, 0, c_cflag, 0, b, b, t[6]]

def open_serial(port, baud, vmin=1, vtime=2):
    fd = os.open(port, os.O_RDWR | os.O_NOCTTY | os.O_NONBLOCK)
    # termios гибкий: 2 слова (много байтов в контроля)
    try:
        import termios as T
        t = T.tcgetattr(fd)
    except Exception as e:
        raise SystemExit("tcgetattr failed: %s" % e)
    # t - список из 5 элементов: [iflag,oflag,cflag,lflag, cc]
    new = set_baud(t, baud)
    new[6][0] = vmin  # VMIN в cc[0]
    new[6][1] = vtime # VTIME в cc[1]
    T.tcsetattr(fd, T.TCSANOW, new)
    return fd

def drain(fd, timeout=0.3):
    """читать всё накопленное до паузы - не нужно, мы в цикле авностями"""

def main():
    import termios as T
    print("Opening %s @ %d, %ds, log=%s" % (PORT, BAUD, SECS, LOG))
    fd = open_serial(PORT, BAUD)
    log = open(LOG, "wb")
    t0 = time.time()
    start = time.time()
    burst = []                      # list of (tmono, byte)
    last_byte = None
    active = False
    nbytes = 0

    def flush_burst():
        if not burst:
            return
        b0 = burst[0][0]
        bN = burst[-1][0]
        data = "".join(chr(b) for _, b in burst)
        line_bits = ["(%8.3f~%8.3f, %5d B, dur=%.1fms)" % (b0, bN, len(burst), (bN-b0)*1000.0)]
        # интервалы между байтами
        gaps = []
        for i in range(1, len(burst)):
            gaps.append((burst[i][0]-burst[i-1][0])*1000.0)
        hi = [(j, "%.1fms" % g) for j, g in enumerate(gaps) if g > STALL*1000]
        if hi:
            line_bits.append(" gaps>%dms: %r" % (STALL*1000, hi))
        out = " ".join(line_bits) + "   " + " ".join("%02x" % b for _, b in burst)
        print(out)
        log.write(("%.3f %s\n" % (time.time()-start, out)).encode("utf-8", "replace"))
        log.flush()
        del burst[:]

    print("Monitoring... Press Ctrl-C to stop.")
    while time.time() - start < SECS:
        r, _, _ = select.select([fd], [], [], 0.05)
        if fd in r:
            chunk = os.read(fd, 256)
            now = time.time() - start
            for b in chunk:
                byte = ord(b)
                nbytes += 1
                if not active:
                    active = True
                    last_byte = now
                burst.append((now, byte))
        else:
            now = time.time() - start
            if active and (now - last_byte > GAP):
                flush_burst()
                active = False
    # финал
    if active:
        flush_burst()
    log.close()
    print("Done. total bytes=%d over %.1fs (rate %.1f B/s)" %
          (nbytes, time.time()-start, nbytes/max(time.time()-start,0.001)))
    print("Log saved to %s" % LOG)

if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        print("\nInterrupted.")