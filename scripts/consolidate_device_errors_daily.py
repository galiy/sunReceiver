#!/usr/bin/env python3
# Разовая чистка/схлопывание sunreceiver.device_errors (2026-11-23).
#
# Новая методика журнала: в пределах одних суток (MSK) для одинаковых
# device/kind/code и одинакового текста ошибки — одна строка; ts = последнее
# время логирования, в msg — накопленное число повторов за сутки.
#
# Скрипт схлопывает уже накопленный шум по тем же правилам:
#   * группировка: (device, kind, code, базовый текст ошибки, сутки MSK);
#   * остаётся одна строка с ts = max(ts) группы;
#   * count = сумма уже накопленных счётчиков (идемпотентно);
#   * msg = базовый текст + " (повторов за сутки: N)".
# По умолчанию dry-run; запись — только с --apply. Перед записью создаётся
# резервная таблица device_errors_backup_20261123.
#
# Подключение: --dsn, либо PG_DSN, либо db.pg из sunReceiver.json.
# Пример (через SSH-туннель к проду):
#   ssh -L 15432:127.0.0.1:5432 root@<прод-сервер> -N
#   python3 scripts/consolidate_device_errors_daily.py --dsn 'postgres://...@127.0.0.1:15432/sunreceiver?sslmode=disable'
#   python3 scripts/consolidate_device_errors_daily.py --apply --dsn '...'
import argparse
import json
import os
import sys
import urllib.parse as up
from collections import defaultdict
from datetime import timezone, timedelta, datetime

try:
    import psycopg
except ImportError:  # старый окружения (python3.12): pg8000
    psycopg = None
    import pg8000.dbapi

HERE = os.path.dirname(os.path.abspath(__file__))
CFG = os.path.abspath(os.path.join(HERE, "..", "sunReceiver.json"))
BACKUP = "sunreceiver.device_errors_backup_20261123"
MSK = timezone(timedelta(hours=3))
SUFFIX = " (повторов за сутки: "


def split_msg(msg):
    if msg.endswith(")"):
        i = msg.rfind(SUFFIX)
        if i >= 0:
            try:
                n = int(msg[i + len(SUFFIX):-1])
                if n >= 1:
                    return msg[:i], n
            except ValueError:
                pass
    return msg, 1


def connect(dsn):
    if psycopg is not None:
        return psycopg.connect(dsn)
    p = up.urlparse(dsn)
    return pg8000.dbapi.connect(
        user=p.username,
        password=p.password,
        host=p.hostname,
        port=p.port,
        database=p.path.lstrip("/"),
    )


def resolve_dsn(arg):
    if arg:
        return arg
    if os.environ.get("PG_DSN"):
        return os.environ["PG_DSN"]
    with open(CFG) as f:
        return json.load(f)["db"]["pg"]


def main():
    ap = argparse.ArgumentParser(description="Схлопывание device_errors по суткам")
    ap.add_argument("--dsn", help="PostgreSQL DSN (иначе PG_DSN или sunReceiver.json)")
    ap.add_argument("--apply", action="store_true", help="выполнить изменения (по умолчанию dry-run)")
    args = ap.parse_args()

    conn = connect(resolve_dsn(args.dsn))
    conn.autocommit = False
    c = conn.cursor()
    try:
        c.execute("LOCK TABLE sunreceiver.device_errors IN SHARE ROW EXCLUSIVE MODE")
        c.execute("SELECT device, name, kind, code, msg, ts FROM sunreceiver.device_errors")
        rows = c.fetchall()
        print("device_errors до чистки:", len(rows))

        groups = {}
        for device, name, kind, code, msg, ts in rows:
            base, n = split_msg(msg)
            if ts.tzinfo is None:
                ts = ts.replace(tzinfo=timezone.utc)
            day = ts.astimezone(MSK).date()
            key = (device, kind, code, base, day)
            g = groups.get(key)
            if g is None:
                groups[key] = {"name": name, "ts": ts, "count": n}
            else:
                g["count"] += n
                if ts >= g["ts"]:
                    g["ts"] = ts
                    g["name"] = name

        print("уникальных групп (device/kind/code/текст/сутки):", len(groups))
        top = sorted(groups.items(), key=lambda kv: kv[1]["count"], reverse=True)[:10]
        for (device, kind, code, base, day), g in top:
            print("  %-22s %-8s %-12s %s: %d" % (device, kind, code, day, g["count"]))

        if not args.apply:
            print("dry-run: изменения откатаны")
            conn.rollback()
            return 0

        c.execute("CREATE TABLE IF NOT EXISTS %s AS TABLE sunreceiver.device_errors" % BACKUP)
        print("резервная копия:", BACKUP)
        c.execute("DELETE FROM sunreceiver.device_errors")
        inserted = 0
        for (device, kind, code, base, day), g in groups.items():
            n = g["count"]
            msg = base + SUFFIX + str(n) + ")"
            c.execute(
                """
                INSERT INTO sunreceiver.device_errors (device, name, kind, code, msg, ts)
                VALUES (%s, %s, %s, %s, %s, %s)
                ON CONFLICT (device, kind, code, ts) DO UPDATE
                SET msg = EXCLUDED.msg, name = EXCLUDED.name
                """,
                (device, g["name"], kind, code, msg, g["ts"].astimezone(timezone.utc)),
            )
            inserted += 1
        c.execute("SELECT count(*) FROM sunreceiver.device_errors")
        print("device_errors после чистки:", c.fetchone()[0], "(вставлено групп:", inserted, ")")
        conn.commit()
        print("изменения зафиксированы")
        return 0
    except Exception:
        conn.rollback()
        raise
    finally:
        c.close()
        conn.close()


if __name__ == "__main__":
    sys.exit(main())
