package httpapi_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	_ "modernc.org/sqlite"

	"github.com/aeza/invoice-keeper/internal/app"
	"github.com/aeza/invoice-keeper/internal/domain"
	"github.com/aeza/invoice-keeper/internal/httpapi"
	"github.com/aeza/invoice-keeper/internal/ratelimit"
	"github.com/aeza/invoice-keeper/internal/session"
	"github.com/aeza/invoice-keeper/internal/storage"
)

// apiClient — минимальный HTTP-клиент поверх httptest: тесты проверяют
// реальные маршруты, коды и JSON, а не вызывают обработчики напрямую.
type apiClient struct {
	t      *testing.T
	app    *fiber.App
	store  *storage.Store
	svc    *app.App
	dbPath string
	token  string
}

// as переключает клиент на другой токен.
func (c *apiClient) as(token string) { c.token = token }

// setRole меняет роль напрямую в базе: продукт не позволяет менять роль через
// API (администратор задаётся в .env), а проверить перечитывание роли нужно.
func (c *apiClient) setRole(login string, role domain.Role) {
	c.t.Helper()

	raw, err := sql.Open("sqlite", c.dbPath)
	if err != nil {
		c.t.Fatalf("второе соединение: %v", err)
	}
	defer raw.Close()

	if _, err := raw.Exec(`UPDATE users SET role = ? WHERE login = ?`, string(role), login); err != nil {
		c.t.Fatalf("смена роли: %v", err)
	}
}

// newAPI собирает API с ограничителем попыток входа.
func newAPI(t *testing.T) *apiClient {
	return newAPIWithLimiter(t, ratelimit.New(10, time.Minute))
}

func newAPIWithLimiter(t *testing.T, limiter *ratelimit.Limiter) *apiClient {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "api.db")
	store, err := storage.New(dbPath)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	svc, err := app.New(store, "Europe/Moscow")
	if err != nil {
		t.Fatalf("app: %v", err)
	}
	if err := svc.Bootstrap("admin", "admin123"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	server := fiber.New()
	httpapi.New(svc, session.New([]byte("test-secret"), 3600e9), limiter).Register(server.Group("/api/v1"))
	return &apiClient{t: t, app: server, store: store, svc: svc, dbPath: dbPath}
}

// userID возвращает идентификатор учётной записи по логину.
func (c *apiClient) userID(login string) int64 {
	c.t.Helper()
	users, err := c.store.ListUsers()
	if err != nil {
		c.t.Fatalf("список пользователей: %v", err)
	}
	for _, u := range users {
		if u.Login == login {
			return u.ID
		}
	}
	c.t.Fatalf("пользователь %q не найден", login)
	return 0
}

// do выполняет запрос и возвращает код и тело.
func (c *apiClient) do(method, path string, body any) (int, []byte) {
	c.t.Helper()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}

	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.app.Test(req, -1)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	return resp.StatusCode, payload
}

// decode разбирает JSON-ответ в map — тестам достаточно структуры.
func (c *apiClient) decode(payload []byte) map[string]any {
	c.t.Helper()
	var out map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		c.t.Fatalf("ответ не JSON: %s", payload)
	}
	return out
}

func (c *apiClient) decodeList(payload []byte) []map[string]any {
	c.t.Helper()
	var out []map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		c.t.Fatalf("ответ не JSON-массив: %s", payload)
	}
	return out
}

// login входит под указанными данными и запоминает токен.
func (c *apiClient) login(login, password string) int {
	code, payload := c.do(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"login": login, "password": password,
	})
	if code == http.StatusOK {
		c.token = c.decode(payload)["token"].(string)
	}
	return code
}

// tokenFor входит под указанными данными и возвращает токен, не меняя
// текущий токен клиента.
func (c *apiClient) tokenFor(login, password string) string {
	c.t.Helper()

	saved := c.token
	defer func() { c.token = saved }()

	if code := c.login(login, password); code != http.StatusOK {
		c.t.Fatalf("вход %s: код %d", login, code)
	}
	return c.token
}

// Без авторизации данные недоступны — это критерий приёмки.
func TestEndpointsRequireAuth(t *testing.T) {
	api := newAPI(t)

	paths := []string{
		"/api/v1/auth/me",
		"/api/v1/recurring-payments",
		"/api/v1/payment-occurrences",
		"/api/v1/payments/summary",
		"/api/v1/users",
	}
	for _, path := range paths {
		if code, _ := api.do(http.MethodGet, path, nil); code != http.StatusUnauthorized {
			t.Errorf("%s без токена: получен %d, ожидался 401", path, code)
		}
	}
}

// Неверный токен не пускает дальше.
func TestInvalidTokenRejected(t *testing.T) {
	api := newAPI(t)
	api.token = "not-a-real-token"

	if code, _ := api.do(http.MethodGet, "/api/v1/payment-occurrences", nil); code != http.StatusUnauthorized {
		t.Fatalf("получен %d, ожидался 401", code)
	}
}

func TestLoginFlow(t *testing.T) {
	api := newAPI(t)

	if code := api.login("admin", "admin123"); code != http.StatusOK {
		t.Fatalf("вход администратора: код %d", code)
	}

	code, payload := api.do(http.MethodGet, "/api/v1/auth/me", nil)
	if code != http.StatusOK {
		t.Fatalf("auth/me: код %d", code)
	}
	me := api.decode(payload)
	if me["login"] != "admin" || me["role"] != "admin" {
		t.Fatalf("auth/me вернул %+v", me)
	}
	if _, leaked := me["password_hash"]; leaked {
		t.Fatal("хеш пароля не должен попадать в ответ")
	}

	if code, _ := api.do(http.MethodPost, "/api/v1/auth/logout", nil); code != http.StatusOK {
		t.Fatalf("logout: код %d", code)
	}
	// После logout тот же токен непригоден.
	if code, _ := api.do(http.MethodGet, "/api/v1/auth/me", nil); code != http.StatusUnauthorized {
		t.Fatalf("после logout получен %d, ожидался 401", code)
	}
}

func TestLoginRejectsWrongPassword(t *testing.T) {
	api := newAPI(t)

	if code := api.login("admin", "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("неверный пароль: код %d, ожидался 401", code)
	}
	if code := api.login("nobody", "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("неизвестный логин: код %d, ожидался 401", code)
	}
}

// Обычный пользователь не может выполнять действия администратора даже через
// прямой вызов API — ключевой критерий приёмки.
func TestUserCannotPerformAdminActions(t *testing.T) {
	api := newAPI(t)

	api.login("admin", "admin123")
	if code, payload := api.do(http.MethodPost, "/api/v1/users", map[string]string{
		"login": "ivan", "password": "secret123",
	}); code != http.StatusCreated {
		t.Fatalf("создание пользователя: код %d (%s)", code, payload)
	}

	api.token = ""
	if code := api.login("ivan", "secret123"); code != http.StatusOK {
		t.Fatalf("вход пользователя: код %d", code)
	}

	adminOnly := []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/v1/users", nil},
		{http.MethodPost, "/api/v1/users", map[string]string{"login": "petr", "password": "secret123"}},
		{http.MethodPost, "/api/v1/recurring-payments", map[string]any{
			"name": "Взлом", "amount": 1, "currency": "RUB", "period": "monthly", "due_date": "2026-12-01",
		}},
		{http.MethodDelete, "/api/v1/recurring-payments/1", nil},
	}
	for _, c := range adminOnly {
		if code, _ := api.do(c.method, c.path, c.body); code != http.StatusForbidden {
			t.Errorf("%s %s пользователем: получен %d, ожидался 403", c.method, c.path, code)
		}
	}
}

// Пользователь имеет право менять отметку об оплате — это его роль по кейсу.
func TestUserCanMarkPaid(t *testing.T) {
	api := newAPI(t)
	api.login("admin", "admin123")

	_, payload := api.do(http.MethodPost, "/api/v1/recurring-payments", map[string]any{
		"name": "Аренда", "amount": 45000, "currency": "RUB",
		"period": "monthly", "due_date": "2026-11-01",
	})
	paymentID := int64(api.decode(payload)["id"].(float64))

	_, payload = api.do(http.MethodGet, fmt.Sprintf("/api/v1/payment-occurrences?payment_id=%d", paymentID), nil)
	occurrences := api.decodeList(payload)
	if len(occurrences) == 0 {
		t.Fatal("периоды не созданы")
	}
	occurrenceID := int64(occurrences[0]["id"].(float64))

	if code, body := api.do(http.MethodPost, "/api/v1/users", map[string]string{
		"login": "ivan", "password": "secret123",
	}); code != http.StatusCreated {
		t.Fatalf("создание пользователя: код %d (%s)", code, body)
	}

	api.token = ""
	api.login("ivan", "secret123")

	code, payload := api.do(http.MethodPatch, fmt.Sprintf("/api/v1/payment-occurrences/%d", occurrenceID),
		map[string]any{"paid_at": "2026-10-30"})
	if code != http.StatusOK {
		t.Fatalf("отметка оплаты пользователем: код %d (%s)", code, payload)
	}
	updated := api.decode(payload)
	if updated["status"] != "paid" || updated["paid_at"] != "2026-10-30" {
		t.Fatalf("ответ после оплаты: %+v", updated)
	}
}

// Контракт API: даты — календарные строки, суммы — основные единицы,
// created_at заполнен сразу после создания.
func TestPaymentResponseContract(t *testing.T) {
	api := newAPI(t)
	api.login("admin", "admin123")

	code, payload := api.do(http.MethodPost, "/api/v1/recurring-payments", map[string]any{
		"name": "Сервер AWS", "amount": 120.5, "currency": "USD",
		"period": "monthly", "due_date": "2026-11-05",
	})
	if code != http.StatusCreated {
		t.Fatalf("создание: код %d (%s)", code, payload)
	}

	p := api.decode(payload)
	if p["first_due_date"] != "2026-11-05" {
		t.Errorf("first_due_date: получено %v, ожидалась строка 2026-11-05", p["first_due_date"])
	}
	if p["amount"] != 120.5 {
		t.Errorf("amount: получено %v, ожидалось 120.5", p["amount"])
	}
	if p["created_at"] == "0001-01-01T00:00:00Z" {
		t.Error("created_at не заполнен после создания платежа")
	}
	if p["active"] != true {
		t.Errorf("active: получено %v", p["active"])
	}
}

// PATCH закрывает три операции: подтверждение, исправление даты и отмену.
func TestOccurrencePatchContract(t *testing.T) {
	api := newAPI(t)
	api.login("admin", "admin123")

	_, payload := api.do(http.MethodPost, "/api/v1/recurring-payments", map[string]any{
		"name": "Подписка", "amount": 8.75, "currency": "EUR",
		"period": "monthly", "due_date": "2026-11-10",
	})
	paymentID := int64(api.decode(payload)["id"].(float64))

	_, payload = api.do(http.MethodGet, fmt.Sprintf("/api/v1/payment-occurrences?payment_id=%d", paymentID), nil)
	occurrenceID := int64(api.decodeList(payload)[0]["id"].(float64))
	path := fmt.Sprintf("/api/v1/payment-occurrences/%d", occurrenceID)

	// Подтверждение.
	_, payload = api.do(http.MethodPatch, path, map[string]any{"paid_at": "2026-11-09"})
	if got := api.decode(payload); got["status"] != "paid" {
		t.Fatalf("подтверждение: %+v", got)
	}

	// Исправление фактической даты.
	_, payload = api.do(http.MethodPatch, path, map[string]any{"paid_at": "2026-11-15"})
	fixed := api.decode(payload)
	if fixed["paid_at"] != "2026-11-15" {
		t.Fatalf("исправление даты: %+v", fixed)
	}
	if fixed["due_date"] != "2026-11-10" {
		t.Fatalf("срок не должен меняться: %+v", fixed)
	}

	// Отмена оплаты: дата очищается, статус пересчитывается.
	_, payload = api.do(http.MethodPatch, path, map[string]any{"paid_at": nil})
	cancelled := api.decode(payload)
	if cancelled["paid_at"] != nil {
		t.Fatalf("отмена должна очищать дату: %+v", cancelled)
	}
	if cancelled["status"] == "paid" {
		t.Fatalf("после отмены статус не может быть paid: %+v", cancelled)
	}
}

// Некорректные входные данные дают 400, а не 500.
func TestValidationErrors(t *testing.T) {
	api := newAPI(t)
	api.login("admin", "admin123")

	cases := []struct {
		name string
		body any
	}{
		{"без названия", map[string]any{"name": "", "amount": 10, "currency": "RUB", "period": "monthly", "due_date": "2026-11-01"}},
		{"нулевая сумма", map[string]any{"name": "Тест", "amount": 0, "currency": "RUB", "period": "monthly", "due_date": "2026-11-01"}},
		{"чужая валюта", map[string]any{"name": "Тест", "amount": 10, "currency": "GBP", "period": "monthly", "due_date": "2026-11-01"}},
		{"плохая дата", map[string]any{"name": "Тест", "amount": 10, "currency": "RUB", "period": "monthly", "due_date": "01.11.2026"}},
		{"плохой период", map[string]any{"name": "Тест", "amount": 10, "currency": "RUB", "period": "daily", "due_date": "2026-11-01"}},
	}
	for _, c := range cases {
		if code, _ := api.do(http.MethodPost, "/api/v1/recurring-payments", c.body); code != http.StatusBadRequest {
			t.Errorf("%s: получен %d, ожидался 400", c.name, code)
		}
	}

	if code, _ := api.do(http.MethodGet, "/api/v1/payment-occurrences/999999", nil); code != http.StatusNotFound {
		t.Errorf("несуществующий период: получен %d, ожидался 404", code)
	}
	if code, _ := api.do(http.MethodGet, "/api/v1/payment-occurrences/abc", nil); code != http.StatusBadRequest {
		t.Errorf("нечисловой id: получен %d, ожидался 400", code)
	}
}

// Администратор защищён и через API: изменить или удалить его нельзя.
func TestAdminProtectedViaAPI(t *testing.T) {
	api := newAPI(t)
	api.login("admin", "admin123")

	_, payload := api.do(http.MethodGet, "/api/v1/users", nil)
	var adminID int64
	for _, u := range api.decodeList(payload) {
		if u["role"] == "admin" {
			adminID = int64(u["id"].(float64))
		}
	}
	if adminID == 0 {
		t.Fatal("администратор не найден в списке")
	}

	if code, _ := api.do(http.MethodPatch, fmt.Sprintf("/api/v1/users/%d", adminID),
		map[string]string{"login": "hacked"}); code != http.StatusBadRequest {
		t.Errorf("изменение администратора: получен %d, ожидался 400", code)
	}
	if code, _ := api.do(http.MethodDelete, fmt.Sprintf("/api/v1/users/%d", adminID), nil); code != http.StatusBadRequest {
		t.Errorf("удаление администратора: получен %d, ожидался 400", code)
	}
}

// Итоги отдаются отдельно по каждой валюте.
func TestSummaryContract(t *testing.T) {
	api := newAPI(t)
	api.login("admin", "admin123")

	for _, p := range []map[string]any{
		{"name": "Аренда", "amount": 45000, "currency": "RUB", "period": "monthly", "due_date": "2026-11-01"},
		{"name": "Сервер", "amount": 120, "currency": "USD", "period": "monthly", "due_date": "2026-11-05"},
		{"name": "Подписка", "amount": 8.75, "currency": "EUR", "period": "monthly", "due_date": "2026-11-10"},
	} {
		if code, body := api.do(http.MethodPost, "/api/v1/recurring-payments", p); code != http.StatusCreated {
			t.Fatalf("создание %v: код %d (%s)", p["name"], code, body)
		}
	}

	code, payload := api.do(http.MethodGet, "/api/v1/payments/summary", nil)
	if code != http.StatusOK {
		t.Fatalf("summary: код %d", code)
	}

	summary := api.decode(payload)
	currencies, _ := summary["by_currency"].([]any)
	if len(currencies) != 3 {
		t.Fatalf("ожидались итоги по трём валютам, получено %d", len(currencies))
	}

	want := map[string]float64{"RUB": 540000, "USD": 1440, "EUR": 105}
	for _, raw := range currencies {
		c := raw.(map[string]any)
		code := c["currency"].(string)
		if total := c["total"].(float64); total != want[code] {
			t.Errorf("%s: итог %v, ожидалось %v", code, total, want[code])
		}
	}

	status, _ := summary["status"].(map[string]any)
	if status["total"].(float64) != 36 {
		t.Errorf("всего периодов: %v, ожидалось 36", status["total"])
	}
}

// Удаление платежа сохраняет историю и убирает его из активных.
func TestStopPaymentContract(t *testing.T) {
	api := newAPI(t)
	api.login("admin", "admin123")

	_, payload := api.do(http.MethodPost, "/api/v1/recurring-payments", map[string]any{
		"name": "Старый", "amount": 10, "currency": "RUB",
		"period": "monthly", "due_date": "2026-11-01",
	})
	paymentID := int64(api.decode(payload)["id"].(float64))

	if code, _ := api.do(http.MethodDelete, fmt.Sprintf("/api/v1/recurring-payments/%d", paymentID), nil); code != http.StatusOK {
		t.Fatalf("прекращение платежа: код %d", code)
	}

	_, payload = api.do(http.MethodGet, "/api/v1/recurring-payments", nil)
	for _, p := range api.decodeList(payload) {
		if int64(p["id"].(float64)) == paymentID {
			t.Fatal("прекращённый платёж не должен попадать в список активных")
		}
	}

	// История периодов сохраняется.
	_, payload = api.do(http.MethodGet, fmt.Sprintf("/api/v1/payment-occurrences?payment_id=%d", paymentID), nil)
	if len(api.decodeList(payload)) == 0 {
		t.Fatal("история периодов должна сохраниться")
	}

	if code, _ := api.do(http.MethodDelete, fmt.Sprintf("/api/v1/recurring-payments/%d", paymentID), nil); code != http.StatusNotFound {
		t.Errorf("повторное прекращение: получен %d, ожидался 404", code)
	}
}

// Создание пользователя: 201, занятый логин — 409, короткий пароль — 400.
func TestUserCreationContract(t *testing.T) {
	api := newAPI(t)
	api.login("admin", "admin123")

	code, payload := api.do(http.MethodPost, "/api/v1/users", map[string]string{
		"login": "ivan", "password": "secret123",
	})
	if code != http.StatusCreated {
		t.Fatalf("создание: код %d (%s)", code, payload)
	}
	if u := api.decode(payload); u["role"] != "user" {
		t.Fatalf("созданный пользователь получил роль %v", u["role"])
	}

	if code, _ := api.do(http.MethodPost, "/api/v1/users", map[string]string{
		"login": "ivan", "password": "secret123",
	}); code != http.StatusConflict {
		t.Errorf("занятый логин: получен %d, ожидался 409", code)
	}

	if code, _ := api.do(http.MethodPost, "/api/v1/users", map[string]string{
		"login": "petr", "password": "123",
	}); code != http.StatusBadRequest {
		t.Errorf("короткий пароль: получен %d, ожидался 400", code)
	}
}

// Перебор пароля ограничивается: после исчерпания попыток API отвечает 429
// и не проверяет пароль дальше.
func TestLoginIsRateLimited(t *testing.T) {
	api := newAPIWithLimiter(t, ratelimit.New(3, time.Minute))

	for i := 0; i < 3; i++ {
		code, _ := api.do(http.MethodPost, "/api/v1/auth/login", map[string]string{
			"login": "admin", "password": "неверный",
		})
		if code != http.StatusUnauthorized {
			t.Fatalf("попытка %d: код %d, ожидался 401", i+1, code)
		}
	}

	code, payload := api.do(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"login": "admin", "password": "admin123",
	})
	if code != http.StatusTooManyRequests {
		t.Fatalf("после лимита: код %d, ожидался 429 (%s)", code, payload)
	}
	if errField := api.decode(payload)["error"]; errField == nil {
		t.Error("ответ 429 не содержит пояснения")
	}
}

// Успешный вход обнуляет счётчик неудач: одна опечатка не блокирует вход.
func TestSuccessfulLoginResetsFailures(t *testing.T) {
	api := newAPIWithLimiter(t, ratelimit.New(2, time.Minute))

	if code, _ := api.do(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"login": "admin", "password": "неверный",
	}); code != http.StatusUnauthorized {
		t.Fatalf("код %d, ожидался 401", code)
	}

	api.login("admin", "admin123") // успешный вход

	for i := 0; i < 2; i++ {
		code, _ := api.do(http.MethodPost, "/api/v1/auth/login", map[string]string{
			"login": "admin", "password": "неверный",
		})
		if code != http.StatusUnauthorized {
			t.Fatalf("попытка %d после сброса: код %d, ожидался 401", i+1, code)
		}
	}
}

// Удалённый пользователь теряет доступ немедленно, хотя его токен ещё не истёк.
func TestDeletedUserLosesAccessImmediately(t *testing.T) {
	api := newAPI(t)
	admin := api.tokenFor("admin", "admin123")

	api.as(admin)
	if code, _ := api.do(http.MethodPost, "/api/v1/users", map[string]string{
		"login": "temp", "password": "temp1234",
	}); code != http.StatusCreated {
		t.Fatalf("создание пользователя: код %d", code)
	}

	tempToken := api.tokenFor("temp", "temp1234")
	api.as(tempToken)
	if code, _ := api.do(http.MethodGet, "/api/v1/payment-occurrences", nil); code != http.StatusOK {
		t.Fatalf("пользователь не имеет доступа: код %d", code)
	}

	// Находим id и удаляем учётную запись администратором.
	api.as(admin)
	code, payload := api.do(http.MethodGet, "/api/v1/users", nil)
	if code != http.StatusOK {
		t.Fatalf("список пользователей: код %d", code)
	}
	var tempID int64
	for _, raw := range api.decodeList(payload) {
		if raw["login"] == "temp" {
			tempID = int64(raw["id"].(float64))
		}
	}
	if tempID == 0 {
		t.Fatal("созданный пользователь не найден в списке")
	}
	if code, _ := api.do(http.MethodDelete, fmt.Sprintf("/api/v1/users/%d", tempID), nil); code != http.StatusOK {
		t.Fatalf("удаление: код %d", code)
	}

	// Тот же токен больше не действует.
	api.as(tempToken)
	if code, _ := api.do(http.MethodGet, "/api/v1/payment-occurrences", nil); code != http.StatusUnauthorized {
		t.Fatalf("токен удалённого пользователя принят: код %d", code)
	}
}

// Понижение администратора действует сразу: старый токен теряет права.
func TestDemotedAdminLosesRightsImmediately(t *testing.T) {
	api := newAPI(t)
	root := api.tokenFor("admin", "admin123")

	// API создаёт только обычных пользователей, поэтому второго администратора
	// заводим как пользователя и повышаем напрямую в базе — так проверяется
	// именно перечитывание роли из хранилища.
	api.as(root)
	code, payload := api.do(http.MethodPost, "/api/v1/users", map[string]string{
		"login": "ex-admin", "password": "exadmin123",
	})
	if code != http.StatusCreated {
		t.Fatalf("создание: код %d (%s)", code, payload)
	}
	api.setRole("ex-admin", domain.RoleAdmin)

	adminToken := api.tokenFor("ex-admin", "exadmin123")
	api.as(adminToken)
	if code, _ := api.do(http.MethodGet, "/api/v1/users", nil); code != http.StatusOK {
		t.Fatalf("администратор не имеет доступа: код %d", code)
	}

	// Понижаем обратно и проверяем, что тот же токен уже не админский.
	api.setRole("ex-admin", domain.RoleUser)
	api.as(adminToken)
	if code, _ := api.do(http.MethodGet, "/api/v1/users", nil); code != http.StatusForbidden {
		t.Fatalf("понижённый администратор сохранил права: код %d", code)
	}
}
