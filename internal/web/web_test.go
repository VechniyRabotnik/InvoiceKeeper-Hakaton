package web_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/VechniyRabotnik/InvoiceKeeper-Hakaton/internal/app"
	"github.com/VechniyRabotnik/InvoiceKeeper-Hakaton/internal/session"
	"github.com/VechniyRabotnik/InvoiceKeeper-Hakaton/internal/storage"
	"github.com/VechniyRabotnik/InvoiceKeeper-Hakaton/internal/web"
)

func newApp(t *testing.T) (*fiber.App, *app.App) {
	t.Helper()

	store, err := storage.New(filepath.Join(t.TempDir(), "web.db"))
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
	if _, err := svc.CreatePayment(app.PaymentInput{
		Name: "Аренда офиса", Amount: 45000, Currency: "RUB",
		Period: "monthly", DueDate: svc.Today().AddDays(2).String(),
	}); err != nil {
		t.Fatalf("payment: %v", err)
	}

	server := fiber.New(fiber.Config{
		Views: web.Templates(),
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			var fe *fiber.Error
			if ok := asFiberError(err, &fe); ok {
				return c.SendStatus(fe.Code)
			}
			return c.SendStatus(http.StatusInternalServerError)
		},
	})
	web.New(svc, session.New([]byte("test-secret"), time.Hour), nil).Register(server)
	return server, svc
}

func asFiberError(err error, target **fiber.Error) bool {
	fe, ok := err.(*fiber.Error)
	if ok {
		*target = fe
	}
	return ok
}

func do(t *testing.T, server *fiber.App, method, path string, form url.Values, cookies ...*http.Cookie) *http.Response {
	t.Helper()

	var req *http.Request
	if form != nil {
		req = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	for _, c := range cookies {
		if c != nil {
			req.AddCookie(c)
		}
	}

	resp, err := server.Test(req, -1)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func login(t *testing.T, server *fiber.App, loginName, password string) *http.Cookie {
	t.Helper()

	resp := do(t, server, http.MethodPost, "/login", url.Values{
		"login": {loginName}, "password": {password},
	})
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("вход %s: код %d, ожидался 302", loginName, resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "ik_session" {
			return c
		}
	}
	t.Fatalf("вход %s: cookie сессии не выдана", loginName)
	return nil
}

func createUser(t *testing.T, server *fiber.App, admin *http.Cookie, loginName, password string) {
	t.Helper()

	resp := do(t, server, http.MethodPost, "/users", url.Values{
		"login": {loginName}, "password": {password},
	}, admin)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("создание %s: код %d", loginName, resp.StatusCode)
	}
}

func userID(t *testing.T, svc *app.App, loginName string) int64 {
	t.Helper()

	users, err := svc.Users()
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range users {
		if u.Login == loginName {
			return u.ID
		}
	}
	t.Fatalf("пользователь %s не найден", loginName)
	return 0
}

func firstOccurrence(t *testing.T, svc *app.App) int64 {
	t.Helper()

	list, err := svc.Occurrences(0)
	if err != nil || len(list) == 0 {
		t.Fatalf("периоды: %v", err)
	}
	return list[0].ID
}

// ── Доступ ────────────────────────────────────────────────────────────────

func TestPagesRequireLogin(t *testing.T) {
	server, _ := newApp(t)

	for _, path := range []string{"/", "/users", "/payments", "/calendar", "/budget"} {
		resp := do(t, server, http.MethodGet, path, nil)
		if resp.StatusCode != http.StatusFound {
			t.Errorf("GET %s без сессии: код %d, ожидался 302", path, resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != "/login" {
			t.Errorf("GET %s: редирект на %q, ожидался /login", path, loc)
		}
	}
}

func TestLoginPageIsPublic(t *testing.T) {
	server, _ := newApp(t)

	resp := do(t, server, http.MethodGet, "/login", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /login: код %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, `name="password"`) {
		t.Error("страница входа не отрендерена")
	}
}

func TestLoginWithWrongPassword(t *testing.T) {
	server, _ := newApp(t)

	resp := do(t, server, http.MethodPost, "/login", url.Values{
		"login": {"admin"}, "password": {"wrong"},
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("код %d, ожидался 401", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "ik_session" && c.Value != "" {
			t.Fatal("при неверном пароле выдана сессия")
		}
	}
	if !strings.Contains(readBody(t, resp), "Неверный логин или пароль") {
		t.Error("нет сообщения об ошибке")
	}
}

// ── Оформление и каркас ───────────────────────────────────────────────────

func TestStylesheetIsServed(t *testing.T) {
	server, _ := newApp(t)

	resp := do(t, server, http.MethodGet, "/static/style.css", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("код %d, ожидался 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/css") {
		t.Errorf("тип контента %q", ct)
	}
	if body := readBody(t, resp); !strings.Contains(body, "--accent") {
		t.Error("похоже, это не тот CSS")
	}
}

func TestPagesLinkStylesheet(t *testing.T) {
	server, _ := newApp(t)
	cookie := login(t, server, "admin", "admin123")

	pages := map[string]*http.Cookie{"/": cookie, "/users": cookie, "/payments": cookie, "/login": nil}
	for path, pageCookie := range pages {
		resp := do(t, server, http.MethodGet, path, nil, pageCookie)
		body := readBody(t, resp)
		if !strings.Contains(body, `href="/static/style.css"`) {
			t.Errorf("%s: ссылка на CSS не найдена", path)
		}
	}
}

// ── Дашборд и Роли ────────────────────────────────────────────────────────

func TestDashboardForAdmin(t *testing.T) {
	server, svc := newApp(t)
	cookie := login(t, server, "admin", "admin123")

	resp := do(t, server, http.MethodGet, "/", nil, cookie)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("код %d", resp.StatusCode)
	}

	body := readBody(t, resp)
	checks := map[string]string{
		"название платежа":  "Аренда офиса",
		"итоги по валютам":  "RUB",
		"ссылка на юзеров":  `href="/users"`,
		"ссылка на шаблоны": `href="/payments"`,
		"сегодняшняя дата":  svc.Today().String(),
	}
	for name, want := range checks {
		if !strings.Contains(body, want) {
			t.Errorf("на дашборде нет %s (%q)", name, want)
		}
	}
}

func TestDashboardForUserHidesAdminControls(t *testing.T) {
	server, _ := newApp(t)
	adminCookie := login(t, server, "admin", "admin123")
	createUser(t, server, adminCookie, "demo", "demo1234")

	cookie := login(t, server, "demo", "demo1234")
	resp := do(t, server, http.MethodGet, "/", nil, cookie)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("код %d", resp.StatusCode)
	}

	body := readBody(t, resp)
	for _, link := range []string{`href="/users"`, `href="/payments"`} {
		if strings.Contains(body, link) {
			t.Errorf("пользователю показана административная ссылка %s", link)
		}
	}
}

// ── Действия с периодами ──────────────────────────────────────────────────

func TestMarkPaidFromDashboard(t *testing.T) {
	server, svc := newApp(t)
	cookie := login(t, server, "admin", "admin123")
	target := firstOccurrence(t, svc)

	resp := do(t, server, http.MethodPost, "/occurrences/"+itoa(target)+"/paid",
		url.Values{"paid_at": {svc.Today().String()}}, cookie)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("код %d, ожидался 302", resp.StatusCode)
	}

	after, err := svc.Occurrence(target)
	if err != nil {
		t.Fatal(err)
	}
	if after.PaidAt == nil || after.Status != "paid" {
		t.Fatalf("период не оплачен: paid_at=%v status=%s", after.PaidAt, after.Status)
	}
}

func TestClearPaymentFromDashboard(t *testing.T) {
	server, svc := newApp(t)
	cookie := login(t, server, "admin", "admin123")

	target := firstOccurrence(t, svc)
	if _, err := svc.SetPaid(target, svc.Today().String()); err != nil {
		t.Fatal(err)
	}

	resp := do(t, server, http.MethodPost, "/occurrences/"+itoa(target)+"/paid?clear=1",
		nil, cookie)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("код %d, ожидался 302", resp.StatusCode)
	}

	after, _ := svc.Occurrence(target)
	if after.PaidAt != nil {
		t.Fatal("отметка об оплате не снята")
	}
}

// ── Персональные страницы ─────────────────────────────────────────────────

func TestPersonalPagesForUser(t *testing.T) {
	server, _ := newApp(t)
	admin := login(t, server, "admin", "admin123")
	createUser(t, server, admin, "ivan", "ivan1234")
	cookie := login(t, server, "ivan", "ivan1234")

	for _, path := range []string{"/calendar", "/budget"} {
		resp := do(t, server, http.MethodGet, path, nil, cookie)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s для пользователя: код %d, ожидался 200", path, resp.StatusCode)
		}
	}
}

// readBody читает тело ответа и закрывает его.
func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("чтение тела: %v", err)
	}
	return string(raw)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
