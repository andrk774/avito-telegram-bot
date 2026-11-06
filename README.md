# Avito → Telegram watcher (Go)

Следит за заданными поисками на Avito и шлёт **новые** объявления в Telegram.
**Без внешних зависимостей** — только стандартная библиотека. Собирается в один
статический бинарник.

## Сборка

```bash
CGO_ENABLED=0 go build -ldflags="-s -w" -o avito-bot .
```

Кросс-компиляция под сервер (пример — Linux amd64):
```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o avito-bot .
```

## Запуск

1. **Бот:** [@BotFather](https://t.me/BotFather) → `/newbot` → токен.
2. **Темы:** опиши в `topics.json` (пример в комплекте). Поля темы:
   - `name` — имя темы в уведомлениях;
   - `url` — ссылка на поиск Avito (как получить — ниже);
   - `max_price` / `min_price` — границы цены (0 = без границы);
   - `include` — слова, которые должны быть в заголовке (все);
   - `exclude` — стоп-слова.
   `interval_seconds` — период опроса (минимум 60, по умолчанию 300).
3. **Старт:**
   ```bash
   BOT_TOKEN=8123456:AAE... ./avito-bot
   ```
4. Напиши боту **`/start`** — он запомнит, куда слать.

Команды: `/list`, `/check` (проверить сейчас), `/status`, `/help`.
`topics.json` перечитывается на каждом цикле — правки подхватываются без
перезапуска. Состояние (что уже видел) — в `state.json` рядом с бинарником.

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
Проверяют парсер карточек, фильтры, сортировку и формат цены.
