package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aeza/invoice-keeper/internal/config"
)

// withEnv выставляет переменные на время теста и восстанавливает их после.
func withEnv(t *testing.T, values map[string]string) {
	t.Helper()

	for k, v := range values {
		old, had := os.LookupEnv(k)
		if err := os.Setenv(k, v); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if had {
				_ = os.Setenv(k, old)
				return
			}
			_ = os.Unsetenv(k)
		})
	}
}

// Без переменных применяются значения по умолчанию — проект запускается
// сразу после клонирования.
func TestDefaults(t *testing.T) {
	// godotenv.Load ищет .env в рабочем каталоге: уводим его в пустой каталог,
	// чтобы локальный файл разработчика не влиял на тест.
	dir := t.TempDir()
	wd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	for _, k := range []string{
		"SERVER_HOST", "SERVER_PORT", "TIMEZONE", "DATABASE_PATH",
		"ADMIN_LOGIN", "ADMIN_PASSWORD", "JWT_SECRET", "SESSION_TTL_HOURS",
		"LOGIN_MAX_ATTEMPTS", "LOGIN_WINDOW_MINUTES",
		"TELEGRAM_BOT_TOKEN", "TELEGRAM_CHAT_ID", "REMINDER_INTERVAL_SECONDS",
	} {
		if old, had := os.LookupEnv(k); had {
			_ = os.Unsetenv(k)
			t.Cleanup(func() { _ = os.Setenv(k, old) })
		}
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Port != 8080 || cfg.Host != "0.0.0.0" {
		t.Errorf("адрес по умолчанию: %s:%d", cfg.Host, cfg.Port)
	}
	if cfg.Timezone != "Europe/Moscow" {
		t.Errorf("часовой пояс по умолчанию: %s", cfg.Timezone)
	}
	if cfg.AdminLogin == "" || cfg.AdminPassword == "" {
		t.Error("администратор по умолчанию не задан")
	}
	if cfg.SessionTTL != 24*time.Hour {
		t.Errorf("TTL сессии: %s", cfg.SessionTTL)
	}
	if cfg.LoginAttempts <= 0 || cfg.LoginWindow <= 0 {
		t.Errorf("ограничение попыток входа: %d/%s", cfg.LoginAttempts, cfg.LoginWindow)
	}
	if cfg.Tick <= 0 {
		t.Errorf("интервал напоминаний: %s", cfg.Tick)
	}
	if cfg.TGToken != "" || cfg.TGChatID != "" {
		t.Error("Telegram включён без настройки")
	}
}

// Переменные окружения переопределяют значения по умолчанию.
func TestEnvOverrides(t *testing.T) {
	withEnv(t, map[string]string{
		"SERVER_HOST":               "127.0.0.1",
		"SERVER_PORT":               "9090",
		"TIMEZONE":                  "UTC",
		"DATABASE_PATH":             "/tmp/custom.db",
		"ADMIN_LOGIN":               "root",
		"ADMIN_PASSWORD":            "s3cret",
		"JWT_SECRET":                "jwt-secret",
		"SESSION_TTL_HOURS":         "2",
		"LOGIN_MAX_ATTEMPTS":        "3",
		"LOGIN_WINDOW_MINUTES":      "1",
		"TELEGRAM_BOT_TOKEN":        "123:abc",
		"TELEGRAM_CHAT_ID":          "-100",
		"REMINDER_INTERVAL_SECONDS": "15",
	})

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Host != "127.0.0.1" || cfg.Port != 9090 {
		t.Errorf("адрес: %s:%d", cfg.Host, cfg.Port)
	}
	if cfg.Timezone != "UTC" {
		t.Errorf("часовой пояс: %s", cfg.Timezone)
	}
	if cfg.DBPath != "/tmp/custom.db" {
		t.Errorf("путь к базе: %s", cfg.DBPath)
	}
	if cfg.AdminLogin != "root" || cfg.AdminPassword != "s3cret" {
		t.Errorf("администратор: %s/%s", cfg.AdminLogin, cfg.AdminPassword)
	}
	if string(cfg.JWTSecret) != "jwt-secret" {
		t.Error("секрет JWT не прочитан")
	}
	if cfg.SessionTTL != 2*time.Hour {
		t.Errorf("TTL: %s", cfg.SessionTTL)
	}
	if cfg.LoginAttempts != 3 || cfg.LoginWindow != time.Minute {
		t.Errorf("лимит входа: %d/%s", cfg.LoginAttempts, cfg.LoginWindow)
	}
	if cfg.TGToken != "123:abc" || cfg.TGChatID != "-100" {
		t.Errorf("Telegram: %s/%s", cfg.TGToken, cfg.TGChatID)
	}
	if cfg.Tick != 15*time.Second {
		t.Errorf("интервал: %s", cfg.Tick)
	}
}

// Неверный часовой пояс — ошибка конфигурации: молча подставлять UTC нельзя,
// иначе все сроки и напоминания сдвинутся.
func TestInvalidTimezoneIsRejected(t *testing.T) {
	withEnv(t, map[string]string{"TIMEZONE": "Марс/Олимп"})

	if _, err := config.Load(); err == nil {
		t.Fatal("некорректный часовой пояс принят")
	}
}

// Пустые учётные данные администратора недопустимы.
func TestEmptyAdminIsRejected(t *testing.T) {
	withEnv(t, map[string]string{"ADMIN_LOGIN": "", "ADMIN_PASSWORD": ""})

	// Пустая строка в окружении трактуется как «не задано» — берётся значение
	// по умолчанию, поэтому проверяем именно невозможность пустого результата.
	cfg, err := config.Load()
	if err != nil {
		return // тоже приемлемый исход
	}
	if cfg.AdminLogin == "" || cfg.AdminPassword == "" {
		t.Fatal("конфигурация с пустым администратором принята")
	}
}

// Некорректное числовое значение не ломает запуск: берётся значение по
// умолчанию, а не ноль.
func TestInvalidNumbersFallBackToDefaults(t *testing.T) {
	withEnv(t, map[string]string{
		"SERVER_PORT":               "не-число",
		"SESSION_TTL_HOURS":         "abc",
		"REMINDER_INTERVAL_SECONDS": "",
	})

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 8080 {
		t.Errorf("порт: %d", cfg.Port)
	}
	if cfg.SessionTTL != 24*time.Hour {
		t.Errorf("TTL: %s", cfg.SessionTTL)
	}
	if cfg.Tick != time.Minute {
		t.Errorf("интервал: %s", cfg.Tick)
	}
}

// .env в рабочем каталоге подхватывается.
func TestLoadsDotEnvFile(t *testing.T) {
	dir := t.TempDir()
	content := "ADMIN_LOGIN=from-file\nADMIN_PASSWORD=file-secret\nSERVER_PORT=7777\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	wd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	// Переменные процесса имеют приоритет над файлом, поэтому чистим их.
	for _, k := range []string{"ADMIN_LOGIN", "ADMIN_PASSWORD", "SERVER_PORT"} {
		if old, had := os.LookupEnv(k); had {
			_ = os.Unsetenv(k)
			t.Cleanup(func() { _ = os.Setenv(k, old) })
		}
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AdminLogin != "from-file" || cfg.AdminPassword != "file-secret" {
		t.Errorf("администратор: %s/%s", cfg.AdminLogin, cfg.AdminPassword)
	}
	if cfg.Port != 7777 {
		t.Errorf("порт: %d", cfg.Port)
	}
}

// Ноль в числовых настройках отклоняется: LIMIT=0 запретил бы любой вход,
// а нулевой интервал напоминаний уронил бы планировщик.
func TestZeroNumbersAreRejected(t *testing.T) {
	cases := map[string]string{
		"SERVER_PORT":               "0",
		"SESSION_TTL_HOURS":         "0",
		"LOGIN_MAX_ATTEMPTS":        "0",
		"LOGIN_WINDOW_MINUTES":      "0",
		"REMINDER_INTERVAL_SECONDS": "0",
	}

	for key, value := range cases {
		t.Run(key, func(t *testing.T) {
			withEnv(t, map[string]string{key: value})
			if _, err := config.Load(); err == nil {
				t.Fatalf("%s=%s принят", key, value)
			}
		})
	}
}
