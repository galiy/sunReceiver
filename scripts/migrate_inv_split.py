#!/usr/bin/env python3
# Этап 3: перенос истории инверторов из ОБЩИХ хранилищ в обособленные марочные
# (deye/sofar) с ключом = серийный номер инвертора (inverter_sn), затем удаление
# общих хранилищ БЕЗ БЭКАПОВ.
#
#   Redis: sunreceiver:current  (field=ip)          -> sunreceiver:<kind>:current        (field=serial)
#          sunreceiver:series:<M> (member=JSON)     -> sunreceiver:<kind>:series:<M>      (тот же score/member)
#   PG:    sunreceiver.averages (ip,...)            -> sunreceiver.<kind>_averages        (serial,...)
#   Удаление: Redis sunreceiver:current, sunreceiver:series:*;  PG DROP TABLE averages.
#
# Марка и серийник берутся из снимков: поле kind ("deye"/"sofar") и inverter_sn.
# Карта ip -> (kind, serial) строится из Redis current (там есть все устройства).
# Запуск с этого (dev) ПК, прямо в БД. Идемпотентно.
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

    # 1) Карта ip -> (kind, serial) из общего current + перенос Redis current.
    ipmap = {}
    moved_cur = 0
    for field, val in r.hscan_iter("sunreceiver:current"):
        ip = field.decode()
        try:
            snap = json.loads(val)
        except Exception:
            continue
        kind, serial = snap.get("kind"), snap.get("inverter_sn")
        if kind not in KINDS or not serial:
            print("  current: пропуск (нет kind/serial):", ip, kind, serial)
            continue
        ipmap[ip] = (kind, serial)
        r.hset(f"sunreceiver:{kind}:current", serial, val)
        moved_cur += 1
    print("Redis current: перенесено полей:", moved_cur)

    # 2) Ряды: sunreceiver:series:<M> -> sunreceiver:<kind>:series:<M>.
    moved_pts = 0
    for key in [k.decode() for k in r.scan_iter(match="sunreceiver:series:*")]:
        month = key[len("sunreceiver:series:"):]
        for member, score in r.zrange(key, 0, -1, withscores=True):
            try:
                snap = json.loads(member)
            except Exception:
                continue
            kind, serial = snap.get("kind"), snap.get("inverter_sn")
            if kind not in KINDS or not serial:
                ipmap.setdefault(snap.get("ip"), (kind, serial))
                continue
            ipmap.setdefault(snap.get("ip"), (kind, serial))
            r.zadd(f"sunreceiver:{kind}:series:{month}", {member: score})
            moved_pts += 1
    print("Redis series: перенесено точек:", moved_pts)

    # 3) PG averages -> <kind>_averages (батч-SQL через временную таблицу-маппинг).
    conn = pg_connect(dsn)
    cur = conn.cursor()
    cur.execute("CREATE TEMP TABLE _ipmap(ip text PRIMARY KEY, kind text, serial text)")
    for ip, (kind, serial) in ipmap.items():
        if kind in KINDS and serial:
            cur.execute(
                "INSERT INTO _ipmap(ip,kind,serial) VALUES (%s,%s,%s) ON CONFLICT DO NOTHING",
                (ip, kind, serial))
    moved_pg = 0
    for kind in KINDS:
        cur.execute(
            f"""INSERT INTO sunreceiver.{kind}_averages (serial, name, ts, device_sn, values)
                SELECT m.serial, a.name, a.ts, a.device_sn, a.values
                FROM sunreceiver.averages a JOIN _ipmap m ON a.ip = m.ip
                WHERE m.kind = %s
                ON CONFLICT (serial, ts) DO UPDATE
                SET name=EXCLUDED.name, device_sn=EXCLUDED.device_sn, values=EXCLUDED.values""",
            (kind,))
        print(f"PG -> {kind}_averages: {cur.rowcount}")
        moved_pg += cur.rowcount
    cur.execute("SELECT count(*) FROM sunreceiver.averages a "
                "LEFT JOIN _ipmap m ON a.ip = m.ip WHERE m.ip IS NULL")
    skipped = cur.fetchone()[0]
    conn.commit()
    print("PG averages: перенесено строк:", moved_pg, "пропущено (нет маппинга):", skipped)

    # 4) Удаление общих хранилищ БЕЗ БЭКАПОВ.
    cur.execute("DROP TABLE IF EXISTS sunreceiver.averages")
    conn.commit()
    cur.close()
    conn.close()
    r.delete("sunreceiver:current")
    for key in [k.decode() for k in r.scan_iter(match="sunreceiver:series:*")]:
        r.delete(key)
    print("Общие хранилища удалены: Redis current/series:*, PG averages.")
    print("done")


if __name__ == "__main__":
    main()
