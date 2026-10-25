# Invoice Keeper - PayDay

Учёт регулярных платежей: веб-панель, REST API и Telegram-бот с напоминаниями.
Аренда, серверы, подписки — все периодические расходы в одном месте, с
отметками об оплате, просрочками и закреплением ответственного за каждый платёж.

## Технологии

| Слой | Что используется |
| --- | --- |
| Язык | Go 1.25 |
| HTTP | Fiber v2, `gofiber/template/html` |
| Хранилище | SQLite (`modernc.org/sqlite`), файл на диске |
| Аутентификация | JWT (`golang-jwt/jwt/v5`) в HttpOnly-cookie, пароли — bcrypt |
| Бот | Telegram Bot API (long polling), без внешних SDK |
| Конфигурация | переменные окружения / `.env` (`joho/godotenv`) |
| UI | серверные HTML-шаблоны + один общий `static/style.css` |
| API-документация | OpenAPI 3.0 (`docs/openapi.yml`) и Swagger UI |

## Возможности

**Веб-панель** (`/`)

- Дашборд: периоды по всем платежам, итоги по валютам, фильтры по статусу,
  ответственному, диапазону и сортировка, отметка «оплачено» с фактической датой.
- Шаблоны платежей (`/payments`): создание, редактирование, пауза и
  возобновление, полное удаление, назначение ответственного, вкладки
  «Активные» / «Удалённые» / «Все», поиск и сортировка.
- Пользователи (`/users`): роли «администратор» и «пользователь», смена пароля,
  привязка Telegram-чата, счётчик платежей у каждой учётной записи.
- Журнал действий (`/journal`) с выгрузкой в CSV.
- Персональные страницы для обычного пользователя: календарь платежей
  (`/calendar`) и бюджет (`/budget`).

**REST API** (`/api/v1`, документация — `/api/v1/docs`)

- Вход и выход, `auth/me`.
- `recurring-payments` — шаблоны: список, создание, правка, назначение,
  пауза/возобновление.
- `payment-occurrences` — периоды: список, правка, отметка об оплате.
- `payments/summary` — итоги и ближайшие сроки.
- `users` — пользователи (только администратор).

**Telegram-бот**

- Напоминания за 3 дня до срока (`ReminderLeadDays`), доставка одному
  получателю на период, повторный тик дублей не создаёт.
- Подписка через `/start`, отказ через `/unsubscribe`; `TELEGRAM_CHAT_ID`
  работает как автоподписка, если подписчиков ещё нет.
- Команды: `/start`, `/help`, `/id`, `/unsubscribe`, `/summary`,
  `/payments [ГГГГ-ММ]`, `/overdue`, `/pay`, `/unpay`,
  `/setdate <ID> <ГГГГ-ММ-ДД>`, `/budget`, `/calendar`.

**Безопасность**

- Пароли — только bcrypt-хеш; минимальная длина пароля 6 символов.
- Сессия — JWT в HttpOnly-cookie; отзыв сессий и смена роли применяются сразу,
  роль читается из базы на каждом запросе.
- Перебор пароля ограничен: `LOGIN_MAX_ATTEMPTS` попыток с адреса за
  `LOGIN_WINDOW_MINUTES`.
- Изменяющие запросы из панели проверяются на происхождение (CSRF-защита по
  `Origin`/`Referer`).
- Шаблоны, CSS и бинарник SQLite не требуют записи на диск, кроме каталога базы.

## Требования

- Go 1.25 или новее — для сборки из исходников.
- Либо Docker и Docker Compose.
- Для напоминаний — токен Telegram-бота; без него приложение работает, а
  планировщик лишь пишет в лог, что отправка отключена.

## Установка и запуск

```bash
git clone https://github.com/VechniyRabotnik/InvoiceKeeper-Hakaton.git
cd InvoiceKeeper-Hakaton
cp .env.example .env        # заполните ADMIN_PASSWORD и JWT_SECRET
go run ./cmd/server
```

Панель — http://localhost:8080, Swagger UI — http://localhost:8080/api/v1/docs.
Первый вход — логином `ADMIN_LOGIN` из `.env`. При первом запуске в пустую базу
добавляются демонстрационные платежи (RUB, USD, EUR).

Сборка бинарника (SQLite чистый Go, CGO не нужен):

```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o invoice-keeper ./cmd/server
./invoice-keeper
```

### Docker

```bash
docker compose up -d --build     # панель на 127.0.0.1:8080, база в ./data
```

Контейнер работает под непривилегированным пользователем, настройки берёт из
`.env`, база лежит в томе `./data` и переживает пересборку образа.

## Настройка окружения

Все параметры читаются из окружения, при наличии подхватывается `.env` рядом с
каталогом запуска. Полный список — в `.env.example`.

| Переменная | По умолчанию | Назначение |
| --- | --- | --- |
| `SERVER_HOST` | `0.0.0.0` | адрес прослушивания |
| `SERVER_PORT` | `8080` | порт HTTP |
| `TIMEZONE` | `Europe/Moscow` | по нему считаются «сегодня» и просрочка |
| `DATABASE_PATH` | `data/invoice-keeper.db` | файл SQLite |
| `ADMIN_LOGIN` | `admin` | логин администратора |
| `ADMIN_PASSWORD` | `admin` | пароль администратора, **обязательно сменить** |
| `JWT_SECRET` | `dev-secret-change-me` | подпись токенов, **обязательно сменить** (`openssl rand -hex 32`) |
| `SESSION_TTL_HOURS` | `24` | срок жизни сессии |
| `LOGIN_MAX_ATTEMPTS` | `10` | попыток входа с одного адреса |
| `LOGIN_WINDOW_MINUTES` | `5` | окно ограничения входа |
| `TELEGRAM_BOT_TOKEN` | пусто | без токена напоминания отключены |
| `TELEGRAM_CHAT_ID` | пусто | автоподписка одного чата |
| `REMINDER_INTERVAL_SECONDS` | `60` | частота проверки напоминаний |

`.env` в git не попадает (`.gitignore`), в Docker-образ — тоже
(`.dockerignore`).

## Проверки

```bash
gofmt -l .                          # форматирование (пусто = всё в порядке)
go vet ./...                        # статический анализ
go mod tidy -diff                   # актуальность go.mod / go.sum
go test ./...                       # тесты
go test -race -count=1 ./...        # тесты с детектором гонок (~2 мин)
CGO_ENABLED=0 go build ./...        # сборка
```

Тесты покрывают домен, хранилище, сервис, REST API, веб-панель, бота, сессии,
ограничение попыток входа и конфигурацию; внешних сервисов они не требуют.

## CI/CD

Конфигурация — [.github/workflows/ci.yml](.github/workflows/ci.yml), запуск на
`push` в `main` / `testing-branch` и на каждый Pull Request.

Задача `check`:

1. `go mod tidy -diff` и `go mod verify` — зависимости в порядке.
2. `gofmt -l` — форматирование.
3. `go vet ./...` — статический анализ.
4. `go test -race -count=1 ./...` — тесты с детектором гонок.
5. `go build` — сборка статического бинарника, артефакт хранится 7 дней.

Задача `docker` — проверка, что `Dockerfile` собирается.

Workflow работает только на чтение репозитория (`permissions: contents: read`)
и не использует секретов: публикация образов и деплой в CI намеренно
отсутствуют.

## Деплой

Целевая среда — VPS с Ubuntu/Debian, поэтому деплой остаётся ручным (в CI его
нет, чтобы доступ к продакшену не зависел от токенов CI):

```bash
git pull
sudo ./deploy/deploy.sh install   # первый раз: пользователь, /opt/invoice-keeper, systemd-юнит
sudo ./deploy/deploy.sh update    # пересобрать бинарник и перезапустить, .env и база не трогаются
./deploy/deploy.sh status | logs | db
```

Приложение работает под отдельным системным пользователем с
`ProtectSystem`/`PrivateTmp`, публичный доступ — через nginx с TLS
(`deploy/nginx/invoice-keeper.conf`, Let's Encrypt).

## Структура

```
cmd/server/          точка входа: конфигурация, маршруты, планировщик
internal/config/     чтение настроек из окружения и .env
internal/domain/     сущности, валюты, периодичность, расчёт статусов
internal/storage/    SQLite: схема, репозитории, миграции
internal/app/        сервис: бизнес-правила, итоги, напоминания, журнал
internal/httpapi/    REST API /api/v1
internal/web/        веб-панель: шаблоны, статика, обработчики
internal/bot/        Telegram-бот и планировщик напоминаний
internal/session/    выпуск и проверка JWT-сессий
internal/ratelimit/  ограничение попыток входа
docs/openapi.yml     спецификация API
deploy/              скрипт установки, systemd-юнит, конфиг nginx
```

## Лицензия

Apache License 2.0 — см. [LICENSE](LICENSE).