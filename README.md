# Avito → Telegram watcher (Go)

[![CI](https://github.com/andrk774/avito-telegram-bot/actions/workflows/ci.yml/badge.svg)](https://github.com/andrk774/avito-telegram-bot/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/github/license/andrk774/avito-telegram-bot)](LICENSE)
[![Go](https://img.shields.io/github/go-mod/go-version/andrk774/avito-telegram-bot)](go.mod)

Следит за заданными поисками на Avito, шлёт **новые** объявления в Telegram и
сообщает о **падении цены** на уже виденных. Темами можно управлять прямо из
чата (кнопки, пошаговое добавление). **Без внешних зависимостей** — только
стандартная библиотека, собирается в один статический бинарник.

## Установка

**Готовый бинарник:** возьми под свою ОС на вкладке
[Releases](https://github.com/andrk774/avito-telegram-bot/releases)
(Linux amd64/arm64, Windows amd64).

**Или собрать из исходников:**
```bash
CGO_ENABLED=0 go build -ldflags="-s -w" -o avito-bot .
```
Кросс-компиляция под сервер (пример — Linux amd64):
```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o avito-bot .
```

## Запуск

1. **Бот:** [@BotFather](https://t.me/BotFather) → `/newbot` → токен.
2. **Старт:**
   ```bash
   BOT_TOKEN=8123456:AAE... ./avito-bot
   ```
3. Напиши боту **`/start`** — он запомнит, куда слать, и покажет кнопки.
4. Добавь темы: кнопка **➕ Добавить** или команда `/add` (бот пошагово спросит
   название, ссылку и максимальную цену). Либо задай начальный список в
   `topics.json` (см. ниже).

### Команды

| Команда | Что делает |
|---|---|
| `/start` | запомнить чат, показать меню с кнопками |
| `/add` | добавить тему пошагово |
| `/list` | список тем с кнопкой 🗑 удаления |
| `/del` | то же, что `/list` — удаление кнопкой |
| `/check` | проверить все темы прямо сейчас |
| `/status` | состояние, число тем, интервал |
| `/menu` | показать кнопки |
| `/cancel` | отменить добавление темы |

### Начальный список тем (`topics.json`)

Необязателен — темы можно завести из чата. Но удобно задать стартовый набор:
```json
{
  "interval_seconds": 300,
  "topics": [
    { "name": "iPhone 13 до 40к",
      "url": "https://www.avito.ru/rossiya/telefony?q=iphone+13",
      "max_price": 40000, "min_price": 10000,
      "include": [], "exclude": ["реплика", "битый"] }
  ]
}
```
Поля: `name` — имя в уведомлениях; `url` — ссылка на поиск Avito; `max_price` /
`min_price` — границы цены (0 = без границы); `include` — слова, которые должны
быть в заголовке; `exclude` — стоп-слова; `interval_seconds` — период опроса
(минимум 60).

> **Важно:** `topics.json` — это **начальный сид**. Он читается один раз, когда
> в `state.json` ещё нет тем. Дальше темами владеет бот (добавление/удаление из
> чата), и всё состояние (темы, виденные объявления и их цены) хранится в
> `state.json` рядом с бинарником. Чтобы перечитать `topics.json` заново —
> удали из `state.json` поле `topics` (или весь файл).

## Как получить URL темы

Открой Avito → задай поиск и все фильтры (регион, категория, цена) → отсортируй
«Сначала новые» → скопируй ссылку из адресной строки. Бот и сам добавит `s=104`.

При первом запуске темы бот **не спамит** старым: запоминает текущую выдачу и
дальше шлёт только появившиеся объявления.

## ⚠️ Про блокировки

Avito защищён антиботом (DataDome):
- **с домашнего IP** — обычно работает;
- **с IP дата-центра (VPS)** — часто капча/403. Бот это ловит и раз в час пишет
  в Telegram, что тему заблокировали.

Если блокирует — варианты: запускать с домашней машины; подключить **жилой
прокси**:
```bash
AVITO_PROXY=http://user:pass@host:port BOT_TOKEN=... ./avito-bot
```
Не занижай `interval_seconds` — частые запросы ускоряют бан. Автосбор формально
против правил Avito; держи частоту низкой.

## Если перестало находить объявления

Avito иногда меняет вёрстку. Чинится в одном месте — функция `parseItems()` и
регулярки над ней в `main.go`. Они опираются на атрибуты `data-marker`
(`item`, `item-title`, `item-price`, `item-address`) — самые стабильные хуки.
Открой выдачу в браузере → F12 → сверь селекторы.

## systemd

`/etc/systemd/system/avito-bot.service`:
```ini
[Unit]
Description=Avito -> Telegram watcher
After=network-online.target

[Service]
WorkingDirectory=/opt/avito-bot
Environment=BOT_TOKEN=8123456:AAE...
# Environment=AVITO_PROXY=http://user:pass@host:port
ExecStart=/opt/avito-bot/avito-bot
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
```
```bash
sudo systemctl daemon-reload && sudo systemctl enable --now avito-bot
journalctl -u avito-bot -f
```

## Тесты

```bash
go test ./...
```
Проверяют парсер карточек, фильтры, сортировку, формат цены, а также логику
хранилища (отслеживание цены и сохранение состояния). CI гоняет `go vet`, тесты
и сборку на каждый push (бейдж вверху).

## Релизы

Пуш тега `vX.Y.Z` запускает сборку бинарников под Linux (amd64/arm64) и
Windows (amd64) и выкладывает их в [Releases](../../releases):
```bash
git tag v1.0.0 && git push origin v1.0.0
```
