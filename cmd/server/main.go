// Команда server поднимает хранилище, сервис, REST API, веб-панель и
// Telegram-бота, затем обслуживает HTTP до сигнала остановки.
package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/logger"

	"github.com/VechniyRabotnik/InvoiceKeeper-Hakaton/internal/app"
	"github.com/VechniyRabotnik/InvoiceKeeper-Hakaton/internal/bot"
	"github.com/VechniyRabotnik/InvoiceKeeper-Hakaton/internal/config"
	"github.com/VechniyRabotnik/InvoiceKeeper-Hakaton/internal/domain"
	"github.com/VechniyRabotnik/InvoiceKeeper-Hakaton/internal/httpapi"
	"github.com/VechniyRabotnik/InvoiceKeeper-Hakaton/internal/ratelimit"
	"github.com/VechniyRabotnik/InvoiceKeeper-Hakaton/internal/session"
	"github.com/VechniyRabotnik/InvoiceKeeper-Hakaton/internal/storage"
	"github.com/VechniyRabotnik/InvoiceKeeper-Hakaton/internal/web"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	store, err := storage.New(cfg.DBPath)
	if err != nil {
		log.Fatalf("storage: %v", err)
	}
	defer store.Close()

	svc, err := app.New(store, cfg.Timezone)
	if err != nil {
		log.Fatalf("app: %v", err)
	}
	if err := svc.Bootstrap(cfg.AdminLogin, cfg.AdminPassword); err != nil {
		log.Fatalf("bootstrap: %v", err)
	}

	if seeded, err := seed(svc); err != nil {
		log.Printf("seed: %v", err)
	} else if seeded {
		log.Println("демонстрационные платежи добавлены (RUB, USD, EUR)")
	}

	if err := autoSubscribe(svc, cfg.TGChatID); err != nil {
		log.Printf("telegram: автоподписка чата: %v", err)
	}

	server, limiter := buildServer(svc, cfg)

	housekeeping := func() {
		if err := store.PurgeExpiredTokens(time.Now()); err != nil {
			log.Printf("purge tokens: %v", err)
		}
		limiter.Sweep()
	}
	housekeeping()

	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				housekeeping()
			case <-stop:
				return
			}
		}
	}()

	notifier := bot.NewTelegram(cfg.TGToken)
	if channel, ok := notifier.(bot.Channel); ok {
		go bot.RunListener(svc, channel, stop)
		bot.SetupCommands(channel)
	}
	go bot.New(svc, notifier, cfg.Tick).Run(stop)
	go syncLoop(svc, cfg.Tick, stop)

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	go func() {
		log.Printf("Invoice Keeper слушает http://%s", addr)
		if err := server.Listen(addr); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("остановка…")
	close(stop)
	if err := server.ShutdownWithTimeout(10 * time.Second); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

// buildServer собирает Fiber: API под /api/v1, панель на корне, Swagger UI.
// Ограничитель попыток входа один на API и панель — перебор пароля одинаково
// доступен с обеих сторон.
func buildServer(svc *app.App, cfg *config.Config) (*fiber.App, *ratelimit.Limiter) {
	engine := web.Templates()
	sessions := session.New(cfg.JWTSecret, cfg.SessionTTL)
	limiter := ratelimit.New(cfg.LoginAttempts, cfg.LoginWindow)

	server := fiber.New(fiber.Config{
		AppName:      "Invoice Keeper",
		Views:        engine,
		ServerHeader: "Invoice Keeper",
		// Без этого за nginx c.IP() отдаёт 127.0.0.1 и все посетители делят
		// один счётчик попыток входа: блокировка одного закрыла бы вход всем.
		ProxyHeader: fiber.HeaderXForwardedFor,
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			var fe *fiber.Error
			if errors.As(err, &fe) {
				code = fe.Code
			}
			if c.Get(fiber.HeaderAccept) == fiber.MIMEApplicationJSON ||
				len(c.Path()) > 5 && c.Path()[:5] == "/api/" {
				return c.Status(code).JSON(fiber.Map{"error": http.StatusText(code)})
			}
			return c.Status(code).SendString(http.StatusText(code))
		},
	})

	server.Use(logger.New(logger.Config{
		Format:     "${time} ${status} ${latency} ${method} ${path}\n",
		TimeFormat: "15:04:05",
	}))
	server.Use(cors.New(cors.Config{
		AllowOrigins: "*",
		AllowHeaders: "Origin, Content-Type, Accept, Authorization",
	}))

	httpapi.New(svc, sessions, limiter).Register(server.Group("/api/v1"))
	web.New(svc, sessions, limiter).Register(server)

	registerDocs(server)
	return server, limiter
}

// seed наполняет пустую базу демонстрационными платежами, чтобы панель и
// напоминания были видны сразу после первого запуска.
func seed(svc *app.App) (bool, error) {
	if has, err := svc.Store().HasPayments(); err != nil || has {
		return false, err
	}

	payments := []app.PaymentInput{
		{Name: "Аренда офиса", Amount: 45000, Currency: domain.RUB, Period: domain.Monthly, DueDate: "2026-11-01"},
		{Name: "Сервер AWS", Amount: 120, Currency: domain.USD, Period: domain.Monthly, DueDate: "2026-11-05"},
		{Name: "Подписка Slack", Amount: 8.75, Currency: domain.EUR, Period: domain.Monthly, DueDate: "2026-11-10"},
	}
	for _, p := range payments {
		if _, err := svc.CreatePayment(p); err != nil {
			return false, fmt.Errorf("%s: %w", p.Name, err)
		}
	}
	return true, nil
}

// autoSubscribe подписывает чат из .env, если подписчиков ещё нет.
//
// TELEGRAM_CHAT_ID остаётся рабочим способом задать получателя, но не мешает
// подписке через /start: как только в базе появится хотя бы один чат, конфиг
// перестаёт влиять на рассылку.
func autoSubscribe(svc *app.App, chatID string) error {
	if chatID == "" {
		return nil
	}
	has, err := svc.HasSubscribers()
	if err != nil || has {
		return err
	}
	if err := svc.SubscribeChat(chatID); err != nil {
		return err
	}
	log.Printf("telegram: чат %s из TELEGRAM_CHAT_ID подписан на напоминания", chatID)
	return nil
}

// syncLoop поддерживает периоды актуальными, пока приложение работает
func syncLoop(svc *app.App, every time.Duration, stop <-chan struct{}) {
	if every < time.Minute {
		every = time.Minute
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := svc.Sync(); err != nil {
				log.Printf("sync periods: %v", err)
			}
		case <-stop:
			return
		}
	}
}
