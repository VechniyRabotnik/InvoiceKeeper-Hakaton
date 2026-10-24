// Package config читает настройки из окружения (.env).
//
// Все обязательные для кейса параметры задаются здесь: администратор,
// единый часовой пояс, Telegram-чат и путь к базе.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	Host     string
	Port     int
	Timezone string
	DBPath   string

	AdminLogin    string
	AdminPassword string

	JWTSecret  []byte
	SessionTTL time.Duration

	// LoginAttempts и LoginWindow ограничивают перебор пароля с одного адреса.
	LoginAttempts int
	LoginWindow   time.Duration

	TGToken  string
	TGChatID string

	Tick time.Duration
}

// Load читает .env (если есть) и переменные окружения.
//
// TELEGRAM_TOKEN и TELEGRAM_CHAT_ID опциональны: без них приложение
// работает, а планировщик лишь пишет в лог, что отправка пропущена. Это
// позволяет запустить проект без бота — важно для проверки жюри.
func Load() (*Config, error) {
	_ = godotenv.Load()

	cfg := &Config{
		Host:          get("SERVER_HOST", "0.0.0.0"),
		Port:          getInt("SERVER_PORT", 8080),
		Timezone:      get("TIMEZONE", "Europe/Moscow"),
		DBPath:        get("DATABASE_PATH", "data/invoice-keeper.db"),
		AdminLogin:    get("ADMIN_LOGIN", "admin"),
		AdminPassword: get("ADMIN_PASSWORD", "admin"),
		JWTSecret:     []byte(get("JWT_SECRET", "dev-secret-change-me")),
		SessionTTL:    time.Duration(getInt("SESSION_TTL_HOURS", 24)) * time.Hour,
		LoginAttempts: getInt("LOGIN_MAX_ATTEMPTS", 10),
		LoginWindow:   time.Duration(getInt("LOGIN_WINDOW_MINUTES", 5)) * time.Minute,
		TGToken:       get("TELEGRAM_BOT_TOKEN", ""),
		TGChatID:      get("TELEGRAM_CHAT_ID", ""),
		Tick:          time.Duration(getInt("REMINDER_INTERVAL_SECONDS", 60)) * time.Second,
	}

	if _, err := time.LoadLocation(cfg.Timezone); err != nil {
		return nil, fmt.Errorf("TIMEZONE %q: %w", cfg.Timezone, err)
	}
	if cfg.AdminLogin == "" || cfg.AdminPassword == "" {
		return nil, fmt.Errorf("ADMIN_LOGIN и ADMIN_PASSWORD обязательны")
	}
	// Числовые значения проверяем явно: молча принятый ноль опаснее ошибки.
	// LOGIN_MAX_ATTEMPTS=0 запретил бы любой вход, а нулевой интервал
	// напоминаний уронил бы планировщик на старте.
	switch {
	case cfg.Port < 1 || cfg.Port > 65535:
		return nil, fmt.Errorf("SERVER_PORT %d вне диапазона 1..65535", cfg.Port)
	case cfg.SessionTTL <= 0:
		return nil, fmt.Errorf("SESSION_TTL_HOURS должен быть больше нуля")
	case cfg.LoginAttempts <= 0:
		return nil, fmt.Errorf("LOGIN_MAX_ATTEMPTS должен быть больше нуля")
	case cfg.LoginWindow <= 0:
		return nil, fmt.Errorf("LOGIN_WINDOW_MINUTES должен быть больше нуля")
	case cfg.Tick <= 0:
		return nil, fmt.Errorf("REMINDER_INTERVAL_SECONDS должен быть больше нуля")
	}
	return cfg, nil
}

func get(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
