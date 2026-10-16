#!/usr/bin/env python3
# Чистка ОБЩИХ хранилищ (sunreceiver:current, sunreceiver:series:*, PG averages)
# от исторических «хвостов», которые не вычистили предыдущие миграции: строки
# устройств, которых больше нет среди опрашиваемых инверторов (напр. legacy
# ip=SofarG2Old, а также старые реальные IP переехавших/обособленных устройств:
# DTS017M, счётчик). После обособления DDS238/МАП/КЭС в общих ключах и
# таблице averages должны оставаться ТОЛЬКО сетевые инверторы (их реальные IP).
#
# Активный набор инверторов = IP из конфига (invertors[]) ∪ поля HASH
# sunreceiver:current (то, что реально наполняется). Всё прочее удаляется.
# Правило проекта: запуск с этого (dev) ПК, прямо в БД. Идемпотентно.
import json
import os
import urllib.parse

import pg8000.dbapi
import redis

HERE = os.path.dirname(os.path.abspath(__file__))
CFG = os.path.join(HERE, "..", "sunReceiver.json")


def load_db():
    with open(os.path.abspath(CFG)) as f:
        d = json.load(f)
    host, port = d["db"]["redis"].split(":")
    ips = {str(inv["ip"]) for inv in d.get("invertors", []) if inv.get("ip")}
    return (host, int(port)), d["db"]["pg"], ips


def pg_connect(dsn):
    u = urllib.parse.urlparse(dsn)
    return pg8000.dbapi.connect(
        user=u.username, password=u.password, host=u.hostname,
        port=u.port or 5432, database=(u.path or "/sunreceiver").lstrip("/"))


def main():
    (rhost, rport), dsn, cfg_ips = load_db()
    r = redis.Redis(host=rhost, port=rport, decode_responses=False)

    live = {f.decode() for f, _ in r.hscan_iter("sunreceiver:current")}
    active = cfg_ips | live
    print("активных инверторов:", len(active), sorted(active))

    # 1) PG averages: удаляем всё, что не активный инвертор.
    conn = pg_connect(dsn)
    cur = conn.cursor()
    cur.execute("SELECT ip, count(*) FROM sunreceiver.averages GROUP BY ip")
    before = dict(cur.fetchall())
    stale_ips = [ip for ip in before if ip not in active]
    if stale_ips:
        cur.execute("DELETE FROM sunreceiver.averages WHERE ip = ANY(%s)", (stale_ips,))
        print("PG averages: удалено строк:", cur.rowcount, "по ip:", sorted(stale_ips))
        conn.commit()
    else:
        print("PG averages: лишних строк нет")
    cur.close()
    conn.close()

    # 2) Redis current: лишние поля.
    stale_fields = [f.decode() for f, _ in r.hscan_iter("sunreceiver:current") if f.decode() not in active]
    if stale_fields:
        r.hdel("sunreceiver:current", *stale_fields)
    print("Redis current: удалено полей:", len(stale_fields), sorted(stale_fields))

    # 3) Redis series: members с неактивным ip.
    total = 0
    for key in [k.decode() for k in r.scan_iter(match="sunreceiver:series:*")]:
        rem = []
        for member in r.zrange(key, 0, -1):
            try:
                snap = json.loads(member)
            except Exception:
                continue
            if str(snap.get("ip", "")) not in active:
                rem.append(member)
        if rem:
            r.zrem(key, *rem)
            total += len(rem)
    print("Redis series: удалено точек:", total)
    print("done")


if __name__ == "__main__":
    main()
