#!/usr/bin/env python3
# Backfill информационной колонки ip в марочных таблицах инверторов
# (deye_averages/sofar_averages): маппинг serial -> ip берётся из Redis current
# (в снимках есть и serial, и ip). Нужна дашборду для привязки размещения
# placeByIP (как было в общей таблице averages). Ключ по-прежнему serial.
# Запуск с этого (dev) ПК. Идемпотентно.
import json
import os
import urllib.parse

import pg8000.dbapi
import redis

HERE = os.path.dirname(os.path.abspath(__file__))
CFG = os.path.join(HERE, "..", "sunReceiver.json")

KINDS = ("deye", "sofar")


def load_db():
    with open(os.path.abspath(CFG)) as f:
        d = json.load(f)["db"]
    host, port = d["redis"].split(":")
    return (host, int(port)), d["pg"]


def pg_connect(dsn):
    u = urllib.parse.urlparse(dsn)
    return pg8000.dbapi.connect(
        user=u.username, password=u.password, host=u.hostname,
        port=u.port or 5432, database=(u.path or "/sunreceiver").lstrip("/"))


def main():
    (rhost, rport), dsn = load_db()
    r = redis.Redis(host=rhost, port=rport, decode_responses=False)
    conn = pg_connect(dsn)
    cur = conn.cursor()
    for kind in KINDS:
        cur.execute("CREATE TEMP TABLE IF NOT EXISTS _m(serial text PRIMARY KEY, ip text)")
        cur.execute("DELETE FROM _m")
        n = 0
        for serial, val in r.hscan_iter(f"sunreceiver:{kind}:current"):
            try:
                snap = json.loads(val)
            except Exception:
                continue
            ip = snap.get("ip")
            if ip:
                cur.execute("INSERT INTO _m(serial,ip) VALUES (%s,%s) ON CONFLICT DO NOTHING",
                            (serial.decode(), ip))
                n += 1
        cur.execute(f"""
            ALTER TABLE sunreceiver.{kind}_averages ADD COLUMN IF NOT EXISTS ip text NOT NULL DEFAULT '';
            UPDATE sunreceiver.{kind}_averages a SET ip = m.ip
            FROM _m m WHERE a.serial = m.serial AND a.ip = ''""")
        conn.commit()
        print(f"{kind}: маппингов {n}, обновлено строк {cur.rowcount}")
    cur.close()
    conn.close()
    print("done")


if __name__ == "__main__":
    main()
