#!/usr/bin/env python3
# Шаг 6: миграция старых ZSET-рядов (sunreceiver:<module>:series:<YYYY-MM>) в
# RedisTimeSeries (sunreceiver:ts:<kind>:<device>:<tag>). Запуск с dev-ПК.
# Идемпотентно (DUPLICATE_POLICY/ON_DUPLICATE LAST). Retention 2 суток — точки
# старше отбросит сам RT (в ZSET и так ~2 суток).
import json
import os
import urllib.parse
from collections import defaultdict

import redis

HERE = os.path.dirname(os.path.abspath(__file__))
CFG = os.path.join(HERE, "..", "sunReceiver.json")

TS_PREFIX = "sunreceiver:ts:"
RETENTION_MS = 172800000
CHUNK = 4000

# module -> ключ устройства (device) и отображаемое имя/ip/device_sn
def key_slug(s):
    for ch in (":", " ", "(", ")", "\n"):
        s = s.replace(ch, "_")
    return s


def ts_key(kind, device, tag):
    return f"{TS_PREFIX}{key_slug(kind)}:{key_slug(device)}:{key_slug(tag)}"


def lbl(v):
    return urllib.parse.quote(str(v), safe="")


def main():
    with open(os.path.abspath(CFG)) as f:
        host, port = json.load(f)["db"]["redis"].split(":")
    r = redis.Redis(host=host, port=int(port), decode_responses=True)

    keys = [k for k in r.scan_iter(match="sunreceiver:*:series:*")]
    print("ZSET keys:", len(keys))
    total_pts = 0
    created = set()
    pipe = r.pipeline(transaction=False)
    pend = 0

    def flush():
        nonlocal pend
        if pend:
            pipe.execute()
            pend = 0

    def add_point(kind, device, name, ip, device_sn, tag, ts_ms, v):
        nonlocal pend
        key = ts_key(kind, device, tag)
        if key not in created:
            created.add(key)
            args = ["TS.CREATE", key, "RETENTION", RETENTION_MS, "DUPLICATE_POLICY", "LAST"]
            labels = [("kind", kind), ("device", device), ("tag", tag),
                      ("name", name), ("ip", ip), ("device_sn", device_sn)]
            pairs = []
            for k, val in labels:
                if val == "" or val is None:
                    continue
                pairs += [k, lbl(val)]
            if pairs:
                args += ["LABELS"] + pairs
            try:
                r.execute_command(*args)
            except redis.exceptions.ResponseError:
                pass  # уже существует (создана живым писателем)
        pipe.execute_command("TS.ADD", key, int(ts_ms), v, "ON_DUPLICATE", "LAST")
        pend += 1
        if pend >= CHUNK:
            flush()

    for key in keys:
        parts = key.split(":")  # sunreceiver <module> series <month>
        if len(parts) < 4:
            continue
        module = parts[1]
        for member, score in r.zrange(key, 0, -1, withscores=True):
            try:
                m = json.loads(member)
            except Exception:
                continue
            ts_ms = int(score) * 1000
            name = m.get("name", "")
            ip = m.get("ip", "") or ""
            if module in ("deye", "sofar"):
                kind, device = module, m.get("inverter_sn", "") or ""
                ds = m.get("device_sn", "") or ""
            elif module == "map":
                kind, device, ds = "map", "map", m.get("device_sn", "") or ""
            elif module == "dds238":
                kind, device, ds = "dds238", "dds238", m.get("device_sn", "") or ""
            elif module == "kes":
                kind, device, ds = "kes", name, m.get("device_sn", "") or ""
            elif module in ("dts017m", "ce308"):
                kind, device, ds = module, name, ""
            elif module == "bms26":
                kind, device = "bms26", m.get("serial", "") or name
                ds = ""
            else:
                continue
            if not device:
                continue
            vals = m.get("values", {}) or {}
            for tag, v in vals.items():
                if isinstance(v, (int, float)):
                    add_point(kind, device, name, ip, ds, tag, ts_ms, v)
                    total_pts += 1
            if module == "bms26":
                for i, v in enumerate(m.get("cells_v", []) or []):
                    if isinstance(v, (int, float)):
                        add_point(kind, device, name, ip, ds, f"cell_{i+1}", ts_ms, v)
                        total_pts += 1
                for i, v in enumerate(m.get("temperatures_c", []) or []):
                    if isinstance(v, (int, float)):
                        add_point(kind, device, name, ip, ds, f"temp_{i+1}", ts_ms, v)
                        total_pts += 1
                for i, v in enumerate(m.get("alarm_bytes", []) or []):
                    if isinstance(v, (int, float)):
                        add_point(kind, device, name, ip, ds, f"alarm_{i+1}", ts_ms, v)
                        total_pts += 1
    flush()
    print("перенесено точек:", total_pts, "серий:", len(created))
    print("done")


if __name__ == "__main__":
    main()
