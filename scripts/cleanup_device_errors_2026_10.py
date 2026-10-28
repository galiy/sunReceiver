#!/usr/bin/env python3
# Разовая чистка истории ошибок sunreceiver.device_errors (2026-10).
#
# Правила (согласованы с оператором):
#   1. F35 («нет сети») — штатное состояние: хранить один переход на эпизод
#      (исторически эпизоды не восстановимы, поэтому оставляем первый за сутки
#      на устройство/код); F35_clear — аналогично.
#   2. Другие аварии инверторов — один код на устройство/сутки (старый баг писал
#      активную аварию повторно после кратковременного пропадания из API).
#   3. no_data МАП/MPPT — кратковременный срыв, не ошибка устройства: удалить
#      исторические записи (новый код пишет только после 5 минут непрерывного
#      отсутствия данных).
#   4. «Устаревшее ПО MPPT (<4.0)» — информационный бит, удалить.
#   5. Нормальные коды AntBMS (balance 2/4, charge 4/15, discharge 11/15,
#      System log) — удалить.
#   6. BMS Warn/Protect: схлопнуть повторы одного кода в пределах 10 минут.
#   7. Остальные МАП-аварии — схлопнуть повторы в пределах 10 минут.
#
# Скрипт идемпотентен. По умолчанию dry-run; запись — только с --apply.
# При --apply создаёт резервную таблицу device_errors_backup_20261028 (если её нет).
#
# Подключение: --dsn, либо PG_DSN, либо db.pg из sunReceiver.json.
# Пример (через SSH-туннель к проду):
#   ssh -L 15432:127.0.0.1:5432 root@<прод-сервер> -N
#   python3 scripts/cleanup_device_errors_2026_10.py \
#       --dsn 'postgres://sunreceiver:...@127.0.0.1:15432/sunreceiver?sslmode=disable'
#   python3 scripts/cleanup_device_errors_2026_10.py --apply --dsn '...'
import argparse
import json
import os
import sys

try:
    import psycopg
except ImportError:  # psycopg2 fallback
    psycopg = None
    import psycopg2

HERE = os.path.dirname(os.path.abspath(__file__))
CFG = os.path.abspath(os.path.join(HERE, "..", "sunReceiver.json"))
BACKUP = "sunreceiver.device_errors_backup_20261028"

NORMAL_BMS = (
    "Balance: code 2",
    "Balance: code 4",
    "Charge: code 4",
    "Charge: code 15",
    "Discharge: code 11",
    "Discharge: code 15",
)

# Удаления: (имя правила, SQL). Все условия — константы, параметров нет.
DELETES = [
    (
        "map: устаревшее ПО MPPT",
        "DELETE FROM sunreceiver.device_errors "
        "WHERE kind = 'map' AND code = 'Устаревшее ПО MPPT (<4.0)'",
    ),
    (
        "map/mppt: no_data",
        "DELETE FROM sunreceiver.device_errors "
        "WHERE kind IN ('map','mppt') AND code = 'no_data'",
    ),
    (
        "bms: нормальные коды AntBMS и System log",
        "DELETE FROM sunreceiver.device_errors "
        "WHERE kind = 'bms' AND (code IN %s OR starts_with(code, 'System log:'))"
        % ("(" + ",".join(["%s"] * len(NORMAL_BMS)) + ")",),
    ),
]

# Схлопывание повторов одного (device,kind,code) в пределах окна.
COLLAPSE = [
    (
        "bms: повторы в пределах 10 минут",
        """
        WITH r AS (
          SELECT ctid,
                 ts - lag(ts) OVER (PARTITION BY device, kind, code ORDER BY ts) AS gap
          FROM sunreceiver.device_errors
          WHERE kind = 'bms'
        )
        DELETE FROM sunreceiver.device_errors d
        USING r
        WHERE d.ctid = r.ctid AND r.gap IS NOT NULL AND r.gap <= interval '10 minutes'
        """,
    ),
    (
        "map: повторы в пределах 10 минут",
        """
        WITH r AS (
          SELECT ctid,
                 ts - lag(ts) OVER (PARTITION BY device, kind, code ORDER BY ts) AS gap
          FROM sunreceiver.device_errors
          WHERE kind = 'map'
        )
        DELETE FROM sunreceiver.device_errors d
        USING r
        WHERE d.ctid = r.ctid AND r.gap IS NOT NULL AND r.gap <= interval '10 minutes'
        """,
    ),
    (
        "inverter: один код на устройство/сутки",
        """
        WITH r AS (
          SELECT ctid,
                 row_number() OVER (
                   PARTITION BY device, kind, code, date_trunc('day', ts)
                   ORDER BY ts
                 ) AS rn
          FROM sunreceiver.device_errors
          WHERE kind = 'inverter'
        )
        DELETE FROM sunreceiver.device_errors d
        USING r
        WHERE d.ctid = r.ctid AND r.rn > 1
        """,
    ),
]


def resolve_dsn(arg):
    if arg:
        return arg
    if os.environ.get("PG_DSN"):
        return os.environ["PG_DSN"]
    with open(CFG) as f:
        return json.load(f)["db"]["pg"]


def connect(dsn):
    if psycopg is not None:
        return psycopg.connect(dsn)
    return psycopg2.connect(dsn)


def main():
    ap = argparse.ArgumentParser(description="Чистка sunreceiver.device_errors (2026-10)")
    ap.add_argument("--dsn", help="PostgreSQL DSN (иначе PG_DSN или sunReceiver.json)")
    ap.add_argument("--apply", action="store_true", help="выполнить изменения (по умолчанию dry-run)")
    args = ap.parse_args()

    conn = connect(resolve_dsn(args.dsn))
    conn.autocommit = False
    cur = conn.cursor()

    cur.execute("SELECT count(*) FROM sunreceiver.device_errors")
    total_before = cur.fetchone()[0]
    print("device_errors до чистки:", total_before)
    print("режим:", "APPLY" if args.apply else "DRY-RUN")

    try:
        # Блокируем запись на время чистки, чтобы не удалить только что
        # записанное живым процессом событие.
        cur.execute("LOCK TABLE sunreceiver.device_errors IN SHARE ROW EXCLUSIVE MODE")
        if args.apply:
            cur.execute(
                "CREATE TABLE IF NOT EXISTS %s AS TABLE sunreceiver.device_errors" % BACKUP
            )
            print("резервная копия:", BACKUP)

        for name, sql in DELETES:
            if name.startswith("bms:"):
                cur.execute(sql, NORMAL_BMS)
            else:
                cur.execute(sql)
            print("  %-42s удалено: %d" % (name, cur.rowcount))

        for name, sql in COLLAPSE:
            cur.execute(sql)
            print("  %-42s удалено: %d" % (name, cur.rowcount))

        cur.execute("SELECT count(*) FROM sunreceiver.device_errors")
        total_after = cur.fetchone()[0]
        print("device_errors после чистки:", total_after, "(удалено:", total_before - total_after, ")")

        if args.apply:
            conn.commit()
            print("изменения зафиксированы")
        else:
            conn.rollback()
            print("dry-run: изменения откатаны")
    except Exception:
        conn.rollback()
        raise
    finally:
        cur.close()
        conn.close()


if __name__ == "__main__":
    sys.exit(main())
