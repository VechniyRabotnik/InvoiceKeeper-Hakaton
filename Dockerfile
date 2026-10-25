# Этап сборки: modernc.org/sqlite — чистый Go, поэтому CGO не нужен и
# получается статический бинарник без системных зависимостей.
FROM golang:1.25-alpine AS build

WORKDIR /src

# Отдельным слоем — чтобы зависимости не перекачивались при правке кода.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/invoice-keeper ./cmd/server

# Этап запуска: ca-certificates нужен для Bot API, tzdata — для TIMEZONE.
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata wget \
    && adduser -D -u 10001 app

WORKDIR /app
COPY --from=build /out/invoice-keeper /app/invoice-keeper

# Каталог базы должен быть доступен неот root-пользователю.
RUN mkdir -p /app/data && chown -R app:app /app
USER app

ENV SERVER_HOST=0.0.0.0 \
    SERVER_PORT=8080 \
    DATABASE_PATH=/app/data/invoice-keeper.db

EXPOSE 8080
VOLUME ["/app/data"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- "http://127.0.0.1:${SERVER_PORT}/login" >/dev/null || exit 1

ENTRYPOINT ["/app/invoice-keeper"]
