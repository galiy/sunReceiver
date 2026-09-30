# Модуль: системный трей и логирование (Windows / POSIX)

Платформенные обёртки жизненного цикла приложения: сворачивание в системный трей
на Windows и запись журнала в файл (на POSIX — no-op, лог идёт в stderr/journald).

## Файлы

| Файл | Платформа | Роль |
|---|---|---|
| `tray_windows.go` | `//go:build windows` | трей (`fyne.io/systray`), иконка `tray.ico` (embed), меню «Закрыть», graceful-quit |
| `tray_posix.go` | `//go:build !windows` | no-op: `runTray` ничего не делает, завершение по SIGINT/SIGTERM |
| `logfile_windows.go` | `//go:build windows` | журнал в `sunReceiver.log` рядом с exe (fallback `%LOCALAPPDATA%`, `%TEMP%`), ротация по размеру |
| `logfile_posix.go` | `//go:build !windows` | `setupLogging()` — no-op |
| `tray.ico` | — | иконка трея (эмбед в бинарник) |
| `main.go` | все | вызывает `setupLogging()` и `runTray`/`waitForQuit` |

## Жизненный цикл и завершение

- `main` вызывает `setupLogging()` до открытия пулов и старта пулеров.
- На Windows `runTray(quit)` запускает `systray.Run` в отдельной горутине; в меню —
  только пункт **«Закрыть»**: он делает `systray.Quit()` и `signalQuit(quit)`.
- `waitForQuit(quit)` дополнительно слушает `SIGINT`/`SIGTERM` (ручной запуск из
  консоли) и тоже сигналит в `quit`.
- `signalQuit` защищён `sync.Once` (`quitOnce`): и пункт меню, и сигнал инициируют
  graceful-завершение ровно один раз (без двойного писателя в канал `quit`).
- Дальше `main` отменяет `stopCtx`, ждёт фоновые горутины (`bgWg`), закрывает
  пулы Redis/PG — см. [../README.md](../README.md) и [storage.md](storage.md).
- На POSIX трея нет: `runTray` — no-op, `waitForQuit` завершает процесс по
  `SIGINT`/`SIGTERM` (штатная остановка systemd-сервиса).

## Логирование (Windows)

Приложение собирается с `-H windowsgui` (подсистема GUI): при запуске из проводника
консоль не показывается, stderr невидим — поэтому лог пишется в файл.

- `setupLogging()` пробует кандидатов по порядку: каталог рядом с исполняемым файлом
  (`filepath.Dir(os.Executable())`, как `sunReceiver.json`) → `%LOCALAPPDATA%\sunReceiver`
  → `%TEMP%`; первый доступный открывается на append.
- Ротация по размеру: при достижении `logMaxSize` (10 МиБ) активный файл
  переименовывается в `sunReceiver.log.1` и открывается новый (`rotatingWriter`).
- Если ни один путь не открылся — лог остаётся в stderr (диагностика деградирует,
  но приложение работает).

## Логирование (POSIX)

`setupLogging()` — no-op: стандартный `log` пишет в stderr; на проде его
собирает **journald** systemd (`journalctl -u sunreceiver.service`), в консоли —
вывод в терминал.

## Сборка

- Windows: `make VERSION=1.2.3 win-x64` → `-H windowsgui` (см. `Makefile`,
  `BLDFLAGS_WIN`), иконка и лог-файл — рядом с exe.
- Linux/macOS: обычная сборка; трей/файловый лог не задействованы.
- Флаг `--version`/`-version` печатает версию, но из-за `-H windowsgui` при запуске
  из проводника stdout не виден (запускать из `cmd` или перенаправлять) — см.
  [../README.md](../README.md), раздел «Сборка, запуск, деплой».

## Связанные документы

- [dashboard.md](dashboard.md) — веб-дашборд и встроенные веб-ассеты (`web/`).
- [../README.md](../README.md) — обзор, сборка релизов, деплой.
