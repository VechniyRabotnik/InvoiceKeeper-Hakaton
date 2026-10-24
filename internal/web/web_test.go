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

// newApp собирает панель с реальными шаблонами: тесты проверяют и разметку,
// и правила доступа, а не только коды ответов.
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

// do выполняет запрос и возвращает ответ.
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

// login возвращает cookie сессии для указанной учётной записи.
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

// createUser заводит обычного пользователя от имени администратора.
func createUser(t *testing.T, server *fiber.App, admin *http.Cookie, loginName, password string) {
	t.Helper()

	resp := do(t, server, http.MethodPost, "/users", url.Values{
		"login": {loginName}, "password": {password},
	}, admin)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("создание %s: код %d", loginName, resp.StatusCode)
	}
}

// userID ищет учётную запись по логину.
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

// firstOccurrence возвращает первый период первого платежа.
func firstOccurrence(t *testing.T, svc *app.App) int64 {
	t.Helper()

	list, err := svc.Occurrences(0)
	if err != nil || len(list) == 0 {
		t.Fatalf("периоды: %v", err)
	}
	return list[0].ID
}

// ── Доступ ────────────────────────────────────────────────────────────────

// Без сессии закрытые страницы недоступны — требование кейса.
func TestPagesRequireLogin(t *testing.T) {
	server, _ := newApp(t)

	for _, path := range []string{"/", "/users", "/payments", "/payments/1/edit"} {
		resp := do(t, server, http.MethodGet, path, nil)
		if resp.StatusCode != http.StatusFound {
			t.Errorf("GET %s без сессии: код %d, ожидался 302", path, resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != "/login" {
			t.Errorf("GET %s: редирект на %q, ожидался /login", path, loc)
		}
	}
}

// Страница входа доступна без сессии.
func TestLoginPageIsPublic(t *testing.T) {
	server, _ := newApp(t)

	resp := do(t, server, http.MethodGet, "/login", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /login: код %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "Invoice Keeper") {
		t.Error("страница входа не отрендерена")
	}
}

// Неверный пароль не выдаёт cookie и показывает ошибку.
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

// Стили отдаются отдельным файлом, а не инлайном в каждой странице.
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

// Страницы подключают внешний CSS и не содержат инлайнового <style>.
func TestPagesLinkStylesheet(t *testing.T) {
	server, _ := newApp(t)
	cookie := login(t, server, "admin", "admin123")

	// /login проверяется без сессии: авторизованного она редиректит на дашборд.
	pages := map[string]*http.Cookie{"/": cookie, "/users": cookie, "/payments": cookie, "/login": nil}
	for path, pageCookie := range pages {
		resp := do(t, server, http.MethodGet, path, nil, pageCookie)
		body := readBody(t, resp)
		if !strings.Contains(body, `href="/static/style.css"`) {
			t.Errorf("%s: ссылка на CSS не найдена", path)
		}
		if strings.Contains(body, "<style>") {
			t.Errorf("%s: в разметке остался инлайновый <style>", path)
		}
	}
}

// ── Дашборд ───────────────────────────────────────────────────────────────

// Администратор видит дашборд с периодами, итогами и колонкой ответственного.
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
		"кнопка оплаты":     "Оплачено",
		"ссылка на юзеров":  `href="/users"`,
		"ссылка на шаблоны": `href="/payments"`,
		"сегодняшняя дата":  svc.Today().String(),
		"подпись статуса":   "Ожидает",
		"колонка хозяина":   "Ответственный",
		"фильтр по людям":   `name="owner"`,
	}
	for name, want := range checks {
		if !strings.Contains(body, want) {
			t.Errorf("на дашборде нет %s (%q)", name, want)
		}
	}
}

// Обычный пользователь видит платежи и отметку оплаты, но не получает
// административных форм и ссылок.
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
	if !strings.Contains(body, "Аренда офиса") {
		t.Error("пользователь не видит платежи")
	}
	if !strings.Contains(body, "Оплачено") {
		t.Error("пользователь не может отметить оплату")
	}
	if strings.Contains(body, "/owner") {
		t.Error("пользователю доступна форма назначения ответственного")
	}
	if strings.Contains(body, `/payments/1/edit`) {
		t.Error("пользователю доступна форма изменения срока периода")
	}
	for _, link := range []string{`href="/users"`, `href="/payments"`} {
		if strings.Contains(body, link) {
			t.Errorf("пользователю показана ссылка %s", link)
		}
	}
}

// Пользователь не может выполнять административные действия панели.
func TestUserCannotUseAdminActions(t *testing.T) {
	server, _ := newApp(t)
	adminCookie := login(t, server, "admin", "admin123")
	createUser(t, server, adminCookie, "demo", "demo1234")
	cookie := login(t, server, "demo", "demo1234")

	cases := []struct {
		method, path string
		form         url.Values
	}{
		{http.MethodGet, "/users", nil},
		{http.MethodGet, "/payments", nil},
		{http.MethodGet, "/payments/1/edit", nil},
		{http.MethodPost, "/users", url.Values{"login": {"x"}, "password": {"xxxxxx"}}},
		{http.MethodPost, "/users/1", url.Values{"login": {"other"}}},
		{http.MethodPost, "/payments", url.Values{
			"name": {"X"}, "amount": {"1"}, "currency": {"RUB"},
			"period": {"monthly"}, "due_date": {"2026-12-01"},
		}},
		{http.MethodPost, "/payments/1", url.Values{"name": {"X"}}},
		{http.MethodPost, "/payments/1/owner", url.Values{"user_id": {"1"}}},
		{http.MethodPost, "/payments/1/stop", nil},
		{http.MethodPost, "/payments/1/resume", nil},
		{http.MethodPost, "/occurrences/1/due", url.Values{"due_date": {"2026-12-01"}}},
		{http.MethodPost, "/occurrences/1/owner", url.Values{"user_id": {"1"}}},
	}
	for _, c := range cases {
		resp := do(t, server, c.method, c.path, c.form, cookie)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s пользователем: код %d, ожидался 403", c.method, c.path, resp.StatusCode)
		}
	}
}

// Администратор не может удалить сам себя — учётная запись из .env защищена.
func TestAdminCannotDeleteSelf(t *testing.T) {
	server, svc := newApp(t)
	cookie := login(t, server, "admin", "admin123")

	admin, err := svc.User(1)
	if err != nil {
		t.Fatal(err)
	}
	resp := do(t, server, http.MethodPost, "/users/"+itoa(admin.ID)+"/delete", nil, cookie)
	if resp.StatusCode == http.StatusFound {
		t.Fatal("администратор удалил сам себя")
	}
	if _, err := svc.Authenticate("admin", "admin123"); err != nil {
		t.Fatalf("администратор больше не может войти: %v", err)
	}
}

// ── Действия с периодами ──────────────────────────────────────────────────

// Отметка оплаты через форму меняет статус периода.
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

// Пустое значение поля снимает отметку об оплате.
func TestClearPaymentFromDashboard(t *testing.T) {
	server, svc := newApp(t)
	cookie := login(t, server, "admin", "admin123")

	target := firstOccurrence(t, svc)
	if _, err := svc.SetPaid(target, svc.Today().String()); err != nil {
		t.Fatal(err)
	}

	resp := do(t, server, http.MethodPost, "/occurrences/"+itoa(target)+"/paid",
		url.Values{"paid_at": {""}}, cookie)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("код %d, ожидался 302", resp.StatusCode)
	}

	after, _ := svc.Occurrence(target)
	if after.PaidAt != nil {
		t.Fatal("отметка об оплате не снята")
	}
}

// Перенос срока периода доступно администратору и не трогает шаблон.
func TestSetOccurrenceDueFromDashboard(t *testing.T) {
	server, svc := newApp(t)
	cookie := login(t, server, "admin", "admin123")

	target := firstOccurrence(t, svc)
	before, err := svc.Occurrence(target)
	if err != nil {
		t.Fatal(err)
	}

	resp := do(t, server, http.MethodPost, "/occurrences/"+itoa(target)+"/due",
		url.Values{"due_date": {"2027-01-15"}}, cookie)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("код %d, ожидался 302", resp.StatusCode)
	}

	after, err := svc.Occurrence(target)
	if err != nil {
		t.Fatal(err)
	}
	if after.DueDate.String() != "2027-01-15" {
		t.Errorf("срок периода %s, ожидался 2027-01-15", after.DueDate)
	}
	payment, err := svc.Payment(before.PaymentID)
	if err != nil {
		t.Fatal(err)
	}
	if payment.FirstDue == after.DueDate {
		t.Error("шаблон изменился вместе со сроком периода")
	}
}

// Некорректная дата переноса отклоняется, а не привод к 500.
func TestSetOccurrenceDueValidation(t *testing.T) {
	server, _ := newApp(t)
	cookie := login(t, server, "admin", "admin123")

	resp := do(t, server, http.MethodPost, "/occurrences/1/due",
		url.Values{"due_date": {"15.01.2027"}}, cookie)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("код %d, ожидался 400", resp.StatusCode)
	}
}

// ── Ответственный ─────────────────────────────────────────────────────────

// Назначение из строки периода закрепляет платеж за пользователем: все его
// периоды наследуют ответственного.
func TestAssignOwnerFromOccurrenceRow(t *testing.T) {
	server, svc := newApp(t)
	cookie := login(t, server, "admin", "admin123")
	createUser(t, server, cookie, "ivan", "ivan1234")
	id := userID(t, svc, "ivan")

	target := firstOccurrence(t, svc)
	resp := do(t, server, http.MethodPost, "/occurrences/"+itoa(target)+"/owner",
		url.Values{"user_id": {itoa(id)}}, cookie)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("код %d, ожидался 302", resp.StatusCode)
	}

	after, err := svc.Occurrence(target)
	if err != nil {
		t.Fatal(err)
	}
	if after.UserID != id || after.UserName != "ivan" {
		t.Errorf("ответственный не назначен: %+v", after)
	}
}

// Назначение из вкладки шаблонов работает напрямую по платежу.
func TestAssignOwnerFromPaymentsPage(t *testing.T) {
	server, svc := newApp(t)
	cookie := login(t, server, "admin", "admin123")
	createUser(t, server, cookie, "ivan", "ivan1234")
	id := userID(t, svc, "ivan")

	payments, err := svc.Payments(true)
	if err != nil || len(payments) == 0 {
		t.Fatalf("платежи: %v", err)
	}

	resp := do(t, server, http.MethodPost, "/payments/"+itoa(payments[0].ID)+"/owner",
		url.Values{"user_id": {itoa(id)}}, cookie)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("код %d, ожидался 302", resp.StatusCode)
	}

	p, err := svc.Payment(payments[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.UserID != id {
		t.Errorf("ответственный %d, ожидался %d", p.UserID, id)
	}
}

// Несуществующий ответственный отклоняется.
func TestAssignUnknownOwner(t *testing.T) {
	server, _ := newApp(t)
	cookie := login(t, server, "admin", "admin123")

	resp := do(t, server, http.MethodPost, "/payments/1/owner", url.Values{"user_id": {"999"}}, cookie)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("код %d, ожидался 400", resp.StatusCode)
	}
}

// Фильтр по ответственному оставляет только его периоды.
func TestDashboardOwnerFilter(t *testing.T) {
	server, svc := newApp(t)
	cookie := login(t, server, "admin", "admin123")
	createUser(t, server, cookie, "ivan", "ivan1234")
	ivan := userID(t, svc, "ivan")

	payments, _ := svc.Payments(true)
	if len(payments) == 0 {
		t.Fatal("базовый платёж не создан")
	}
	if _, err := svc.CreatePayment(app.PaymentInput{
		Name: "Сервер Ивана", Amount: 100, Currency: "RUB",
		Period: "monthly", DueDate: svc.Today().AddDays(3).String(), UserID: ivan,
	}); err != nil {
		t.Fatal(err)
	}

	all := readBody(t, do(t, server, http.MethodGet, "/", nil, cookie))
	if !strings.Contains(all, "Аренда офиса") || !strings.Contains(all, "Сервер Ивана") {
		t.Error("без фильтра оба платежа должны быть видны")
	}

	mine := readBody(t, do(t, server, http.MethodGet, "/?owner="+itoa(ivan), nil, cookie))
	if !strings.Contains(mine, "Сервер Ивана") {
		t.Error("фильтр не показал платёж ответственного")
	}
	if strings.Contains(mine, "Аренда офиса") {
		t.Error("фильтр показал чужой платёж")
	}

	none := readBody(t, do(t, server, http.MethodGet, "/?owner=none", nil, cookie))
	if !strings.Contains(none, "Аренда офиса") || strings.Contains(none, "Сервер Ивана") {
		t.Error("фильтр «без ответственного» работает неверно")
	}
}

// Итоги на дашборде считаются в том же фильтре, что и таблица.
func TestDashboardSummaryFollowsFilter(t *testing.T) {
	server, svc := newApp(t)
	cookie := login(t, server, "admin", "admin123")
	createUser(t, server, cookie, "ivan", "ivan1234")
	ivan := userID(t, svc, "ivan")

	if _, err := svc.CreatePayment(app.PaymentInput{
		Name: "Сервер Ивана", Amount: 100, Currency: "USD",
		Period: "monthly", DueDate: svc.Today().AddDays(3).String(), UserID: ivan,
	}); err != nil {
		t.Fatal(err)
	}

	all := readBody(t, do(t, server, http.MethodGet, "/", nil, cookie))
	if !strings.Contains(all, "USD") || !strings.Contains(all, "RUB") {
		t.Error("без фильтра итогов по обеим валютам нет")
	}

	mine := readBody(t, do(t, server, http.MethodGet, "/?owner="+itoa(ivan), nil, cookie))
	if !strings.Contains(mine, "USD") {
		t.Error("в фильтре не осталось итогов ответственного")
	}
	if strings.Contains(mine, "RUB ·") {
		t.Error("чужие валюты попали в итоги фильтра")
	}
}

// ── Шаблоны платежей ──────────────────────────────────────────────────────

// Вкладка шаблонов показывает форму создания и все шаблоны с ответственным.
func TestPaymentsPageForAdmin(t *testing.T) {
	server, svc := newApp(t)
	cookie := login(t, server, "admin", "admin123")

	resp := do(t, server, http.MethodGet, "/payments", nil, cookie)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("код %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	for _, want := range []string{
		`action="/payments"`, "Аренда офиса", "Ответственный",
		"Периодичность", "Ежемесячно", "/edit",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("на вкладке шаблонов нет %q", want)
		}
	}
	_ = svc
}

// Создание платежа из формы добавляет периоды и закрепляет ответственного.
func TestCreatePaymentFromForm(t *testing.T) {
	server, svc := newApp(t)
	cookie := login(t, server, "admin", "admin123")
	createUser(t, server, cookie, "ivan", "ivan1234")
	ivan := userID(t, svc, "ivan")

	before, _ := svc.Payments(true)
	resp := do(t, server, http.MethodPost, "/payments", url.Values{
		"name": {"Сервер AWS"}, "amount": {"120"}, "currency": {"USD"},
		"period": {"monthly"}, "due_date": {"2026-11-05"}, "user_id": {itoa(ivan)},
	}, cookie)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("код %d, ожидался 302", resp.StatusCode)
	}

	after, _ := svc.Payments(true)
	if len(after) != len(before)+1 {
		t.Fatalf("платежей %d, ожидалось %d", len(after), len(before)+1)
	}

	var createdID int64
	for i := range after {
		if after[i].Name == "Сервер AWS" {
			createdID = after[i].ID
			if after[i].UserID != ivan {
				t.Errorf("ответственный %d, ожидался %d", after[i].UserID, ivan)
			}
		}
	}
	if createdID == 0 {
		t.Fatal("платёж не найден после создания")
	}

	resp = do(t, server, http.MethodGet, "/payments/"+itoa(createdID)+"/edit", nil, cookie)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("страница созданного платежа не открылась: код %d", resp.StatusCode)
	}
}

// Некорректная сумма отклоняется с понятным кодом, а не 500.
func TestCreatePaymentValidation(t *testing.T) {
	server, _ := newApp(t)
	cookie := login(t, server, "admin", "admin123")

	resp := do(t, server, http.MethodPost, "/payments", url.Values{
		"name": {"Плохой"}, "amount": {"не число"}, "currency": {"RUB"},
		"period": {"monthly"}, "due_date": {"2026-11-05"},
	}, cookie)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("код %d, ожидался 400", resp.StatusCode)
	}
}

// Страница редактирования показывает текущие значения шаблона.
func TestPaymentEditPageShowsValues(t *testing.T) {
	server, svc := newApp(t)
	cookie := login(t, server, "admin", "admin123")

	payments, _ := svc.Payments(true)
	resp := do(t, server, http.MethodGet, "/payments/"+itoa(payments[0].ID)+"/edit", nil, cookie)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("код %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	for _, want := range []string{"Аренда офиса", `value="45000.00"`, "Изменение платежа"} {
		if !strings.Contains(body, want) {
			t.Errorf("на странице редактирования нет %q", want)
		}
	}
}

// Сохранение меняет шаблон и пересобирает будущие периоды.
func TestUpdatePaymentRecalculatesOccurrences(t *testing.T) {
	server, svc := newApp(t)
	cookie := login(t, server, "admin", "admin123")

	payments, _ := svc.Payments(true)
	id := payments[0].ID
	before, err := svc.Occurrences(id)
	if err != nil || len(before) == 0 {
		t.Fatalf("периоды: %v", err)
	}

	resp := do(t, server, http.MethodPost, "/payments/"+itoa(id), url.Values{
		"name": {"Аренда офиса"}, "amount": {"50000"}, "currency": {"RUB"},
		"period": {"quarterly"}, "due_date": {svc.Today().AddDays(2).String()}, "user_id": {"0"},
	}, cookie)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("код %d, ожидался 302", resp.StatusCode)
	}

	updated, err := svc.Payment(id)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Period != "quarterly" || updated.Amount.Major() != 50000 {
		t.Errorf("шаблон не обновлён: %+v", updated)
	}

	after, err := svc.Occurrences(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) < 2 {
		t.Fatalf("периодов %d, ожидалось не меньше 2", len(after))
	}
	if gap := after[0].DueDate.DaysUntil(after[1].DueDate); gap != 91 && gap != 92 {
		t.Errorf("шаг между периодами %d дн., ожидался квартал", gap)
	}
}

// Остановленный платёж можно возобновить.
func TestStopAndResumePayment(t *testing.T) {
	server, svc := newApp(t)
	cookie := login(t, server, "admin", "admin123")

	payments, _ := svc.Payments(true)
	id := payments[0].ID

	if resp := do(t, server, http.MethodPost, "/payments/"+itoa(id)+"/stop", nil, cookie); resp.StatusCode != http.StatusFound {
		t.Fatalf("остановка: код %d", resp.StatusCode)
	}
	active, _ := svc.Payments(true)
	for _, p := range active {
		if p.ID == id {
			t.Fatal("остановленный платёж остался в активных")
		}
	}

	if resp := do(t, server, http.MethodPost, "/payments/"+itoa(id)+"/resume", nil, cookie); resp.StatusCode != http.StatusFound {
		t.Fatalf("возобновление: код %d", resp.StatusCode)
	}
	active, _ = svc.Payments(true)
	found := false
	for _, p := range active {
		if p.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatal("платёж не возобновлён")
	}
}

// Остановленный шаблон виден на вкладке и не уходит в активные.
func TestPaymentsPageShowsStopped(t *testing.T) {
	server, svc := newApp(t)
	cookie := login(t, server, "admin", "admin123")

	payments, _ := svc.Payments(true)
	id := payments[0].ID
	do(t, server, http.MethodPost, "/payments/"+itoa(id)+"/stop", nil, cookie)

	body := readBody(t, do(t, server, http.MethodGet, "/payments", nil, cookie))
	if !strings.Contains(body, "остановлен") {
		t.Error("остановленный шаблон не помечен на вкладке")
	}
	if !strings.Contains(body, "Возобновить") {
		t.Error("нет кнопки возобновления")
	}
}

// ── Пользователи ──────────────────────────────────────────────────────────

// Вкладка пользователей показывает форму создания и счётчик платежей.
func TestUsersPageForAdmin(t *testing.T) {
	server, svc := newApp(t)
	cookie := login(t, server, "admin", "admin123")
	createUser(t, server, cookie, "ivan", "ivan1234")

	payments, _ := svc.Payments(true)
	resp := do(t, server, http.MethodPost, "/payments/"+itoa(payments[0].ID)+"/owner",
		url.Values{"user_id": {itoa(userID(t, svc, "ivan"))}}, cookie)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("назначение: код %d", resp.StatusCode)
	}

	resp = do(t, server, http.MethodGet, "/users", nil, cookie)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("код %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	for _, want := range []string{"ivan", `action="/users"`, "Платежей", "Новый пароль", `href="/?owner=`} {
		if !strings.Contains(body, want) {
			t.Errorf("на вкладке пользователей нет %q", want)
		}
	}
}

// Смена логина доступна прямо из таблицы.
func TestUpdateUserLogin(t *testing.T) {
	server, svc := newApp(t)
	cookie := login(t, server, "admin", "admin123")
	createUser(t, server, cookie, "ivan", "ivan1234")
	id := userID(t, svc, "ivan")

	resp := do(t, server, http.MethodPost, "/users/"+itoa(id),
		url.Values{"login": {"ivanov"}, "password": {""}}, cookie)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("код %d, ожидался 302", resp.StatusCode)
	}
	if _, err := svc.Authenticate("ivanov", "ivan1234"); err != nil {
		t.Fatalf("вход по новому логину не работает: %v", err)
	}
}

// Пустой пароль оставляет прежний, заполненный — меняет.
func TestUpdateUserPassword(t *testing.T) {
	server, svc := newApp(t)
	cookie := login(t, server, "admin", "admin123")
	createUser(t, server, cookie, "ivan", "ivan1234")
	id := userID(t, svc, "ivan")

	if _, err := svc.UpdateUser(id, "", ""); err != nil {
		t.Fatalf("пустая форма: %v", err)
	}
	if _, err := svc.Authenticate("ivan", "ivan1234"); err != nil {
		t.Fatalf("пустой пароль сбросил прежний: %v", err)
	}

	resp := do(t, server, http.MethodPost, "/users/"+itoa(id),
		url.Values{"login": {"ivan"}, "password": {"fresh123"}}, cookie)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("код %d, ожидался 302", resp.StatusCode)
	}
	if _, err := svc.Authenticate("ivan", "fresh123"); err != nil {
		t.Fatalf("пароль не обновлён: %v", err)
	}
}

// Занятый логин отклоняется, а не создаёт дубль.
func TestUpdateUserDuplicateLogin(t *testing.T) {
	server, svc := newApp(t)
	cookie := login(t, server, "admin", "admin123")
	createUser(t, server, cookie, "ivan", "ivan1234")
	createUser(t, server, cookie, "petr", "petr1234")
	id := userID(t, svc, "petr")

	resp := do(t, server, http.MethodPost, "/users/"+itoa(id),
		url.Values{"login": {"ivan"}, "password": {""}}, cookie)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("код %d, ожидался 400", resp.StatusCode)
	}
}

// Удалённый пользователь теряет доступ к панели сразу: cookie перестаёт
// действовать, хотя срок её жизни ещё не истёк.
func TestDeletedUserLosesPanelAccess(t *testing.T) {
	server, svc := newApp(t)
	adminCookie := login(t, server, "admin", "admin123")
	createUser(t, server, adminCookie, "temp", "temp1234")
	userCookie := login(t, server, "temp", "temp1234")

	if resp := do(t, server, http.MethodGet, "/", nil, userCookie); resp.StatusCode != http.StatusOK {
		t.Fatalf("пользователь не имеет доступа: код %d", resp.StatusCode)
	}

	id := userID(t, svc, "temp")
	if err := svc.DeleteUser(id); err != nil {
		t.Fatalf("удаление: %v", err)
	}

	resp := do(t, server, http.MethodGet, "/", nil, userCookie)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("удалённый пользователь сохранил доступ: код %d", resp.StatusCode)
	}
}

// ── Сессия и CSRF ─────────────────────────────────────────────────────────

// Выход отзывает сессию: старая cookie больше не даёт доступ.
func TestLogoutRevokesSession(t *testing.T) {
	server, _ := newApp(t)
	cookie := login(t, server, "admin", "admin123")

	resp := do(t, server, http.MethodPost, "/logout", nil, cookie)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("код %d, ожидался 302", resp.StatusCode)
	}

	resp = do(t, server, http.MethodGet, "/", nil, cookie)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("после выхода доступ получен: код %d", resp.StatusCode)
	}
}

// Подделанная cookie не даёт доступ.
func TestTamperedCookieIsRejected(t *testing.T) {
	server, _ := newApp(t)

	resp := do(t, server, http.MethodGet, "/", nil, &http.Cookie{
		Name: "ik_session", Value: "pодделка.подпись.значение",
	})
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("код %d, ожидался редирект на вход", resp.StatusCode)
	}
}

// Cookie сессии должна быть HttpOnly: токен недоступен из JS.
func TestSessionCookieIsHttpOnly(t *testing.T) {
	server, _ := newApp(t)
	cookie := login(t, server, "admin", "admin123")

	if !cookie.HttpOnly {
		t.Error("cookie сессии не помечена HttpOnly")
	}
	if cookie.Path != "/" {
		t.Errorf("путь cookie %q, ожидался /", cookie.Path)
	}
}

// Изменяющие запросы с чужого источника отбрасываются: cookie браузер
// прикладывает автоматически, поэтому одной сессии для CSRF мало.
func TestCrossOriginPostIsRejected(t *testing.T) {
	server, _ := newApp(t)
	adminCookie := login(t, server, "admin", "admin123")

	post := func(origin string) int {
		form := url.Values{"paid_at": {"2026-11-01"}}
		req := httptest.NewRequest(http.MethodPost, "/occurrences/1/paid", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", origin)
		req.AddCookie(adminCookie)
		resp, err := server.Test(req, -1)
		if err != nil {
			t.Fatalf("запрос: %v", err)
		}
		return resp.StatusCode
	}

	if code := post("https://evil.example"); code != http.StatusForbidden {
		t.Errorf("чужой источник: код %d, ожидался 403", code)
	}
	if code := post("http://evil.example:8080"); code != http.StatusForbidden {
		t.Errorf("чужой источник с портом: код %d, ожидался 403", code)
	}
	// Свой источник проходит: порт не должен влиять на сравнение.
	if code := post("http://example.com:8080"); code != http.StatusFound {
		t.Errorf("свой источник: код %d, ожидался 302", code)
	}
}

// POST без Origin и Referer (curl, серверный клиент) не блокируется: CSRF
// возможен только в браузере, который эти заголовки присылает.
func TestPostWithoutOriginIsAllowed(t *testing.T) {
	server, _ := newApp(t)
	adminCookie := login(t, server, "admin", "admin123")

	resp := do(t, server, http.MethodPost, "/occurrences/1/paid", url.Values{
		"paid_at": {"2026-11-01"},
	}, adminCookie)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("код %d, ожидался 302", resp.StatusCode)
	}
}

// Шаблоны встроены в бинарник: панель не зависит от рабочего каталога.
func TestTemplatesAreEmbedded(t *testing.T) {
	engine := web.Templates()

	for _, name := range []string{"login", "dashboard", "users", "payments", "payment-edit"} {
		if err := engine.Render(io.Discard, name, map[string]any{}); err != nil &&
			strings.Contains(err.Error(), "does not exist") {
			t.Errorf("шаблон %s не встроен: %v", name, err)
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
