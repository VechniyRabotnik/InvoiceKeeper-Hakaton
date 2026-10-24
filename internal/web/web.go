// Package web — серверный веб-интерфейс Invoice Keeper.
//
// Панель не дублирует бизнес-логику: она вызывает тот же сервис, что и API, и
// использует те же правила доступа. Авторизация — HttpOnly-cookie с тем же
// JWT, что выдаёт API: браузер не хранит токен в JS, а любая страница без
// валидной сессии редиректит на вход.
//
// Разметка разделена по страницам (templates/*.html), оформление живёт в
// static/style.css, общий каркас — в layout.html.
package web

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/VechniyRabotnik/InvoiceKeeper-Hakaton/internal/app"
	"github.com/VechniyRabotnik/InvoiceKeeper-Hakaton/internal/domain"
	"github.com/VechniyRabotnik/InvoiceKeeper-Hakaton/internal/ratelimit"
	"github.com/VechniyRabotnik/InvoiceKeeper-Hakaton/internal/session"
)

const sessionCookie = "ik_session"

// Значения фильтра «Ответственный» на дашборде. filterAll = app.NoUserFilter:
// «нет фильтра» и «без ответственного» — разные выборки.
const (
	filterAll    = app.NoUserFilter
	filterNone   = int64(0)
	ownerAll     = "all"
	ownerNone    = "none"
	ownerDefault = ownerAll
)

// Handler — веб-панель.
type Handler struct {
	svc      *app.App
	sessions *session.Service
	limiter  *ratelimit.Limiter
}

// New создаёт панель. limiter может быть nil — тогда ограничение попыток
// входа отключено.
func New(svc *app.App, sessions *session.Service, limiter *ratelimit.Limiter) *Handler {
	return &Handler{svc: svc, sessions: sessions, limiter: limiter}
}

// Register развешивает маршруты панели. Guard-ы указываются в каждом маршруте
// явно: middleware группы в Fiber накрывает весь префикс, а нам нужен разный
// доступ к разным страницам.
func (h *Handler) Register(r fiber.Router) {
	Static(r)

	r.Get("/login", h.LoginPage)
	r.Post("/login", h.LoginSubmit)
	r.Post("/logout", h.Logout)

	// Дашборд: периоды, итоги и отметки об оплате.
	r.Get("/", h.requireAuth, h.Dashboard)
	r.Post("/occurrences/:id/paid", h.requireAuth, h.MarkPaid)
	r.Post("/occurrences/:id/due", h.requireAuth, h.requireAdmin, h.SetOccurrenceDue)
	r.Post("/occurrences/:id/owner", h.requireAuth, h.requireAdmin, h.AssignOccurrenceOwner)

	// Шаблоны платежей: создание, редактирование, ответственный, пауза.
	r.Get("/payments", h.requireAuth, h.requireAdmin, h.PaymentsPage)
	r.Get("/payments/:id/edit", h.requireAuth, h.requireAdmin, h.PaymentEditPage)
	r.Post("/payments", h.requireAuth, h.requireAdmin, h.CreatePayment)
	r.Post("/payments/:id", h.requireAuth, h.requireAdmin, h.UpdatePayment)
	r.Post("/payments/:id/owner", h.requireAuth, h.requireAdmin, h.AssignPaymentOwner)
	r.Post("/payments/:id/stop", h.requireAuth, h.requireAdmin, h.StopPayment)
	r.Post("/payments/:id/resume", h.requireAuth, h.requireAdmin, h.ResumePayment)
	r.Post("/payments/:id/delete", h.requireAuth, h.requireAdmin, h.DeletePayment)

	// Пользователи.
	r.Get("/users", h.requireAuth, h.requireAdmin, h.UsersPage)
	r.Post("/users", h.requireAuth, h.requireAdmin, h.CreateUser)
	r.Post("/users/:id", h.requireAuth, h.requireAdmin, h.UpdateUser)
	r.Post("/users/:id/chat", h.requireAuth, h.requireAdmin, h.BindTelegramChat)
	r.Post("/users/:id/delete", h.requireAuth, h.requireAdmin, h.DeleteUser)
}

// ── Сессия ────────────────────────────────────────────────────────────────

// session разбирает cookie, проверяет отзыв сессии и актуальность учётной
// записи. Роль берётся из базы: понижение администратора или удаление
// пользователя действуют немедленно, а не после истечения cookie.
func (h *Handler) session(c *fiber.Ctx) *session.Claims {
	raw := c.Cookies(sessionCookie)
	if raw == "" {
		return nil
	}
	claims, err := h.sessions.Verify(raw)
	if err != nil {
		return nil
	}
	if revoked, err := h.svc.SessionRevoked(claims.ID); err != nil || revoked {
		return nil
	}

	user, err := h.svc.User(claims.UserID)
	if err != nil {
		return nil
	}
	claims.Login, claims.Role = user.Login, string(user.Role)
	return claims
}

// requireAuth пускает только с валидной сессией, иначе — на страницу входа.
func (h *Handler) requireAuth(c *fiber.Ctx) error {
	claims := h.session(c)
	if claims == nil {
		if c.Method() == fiber.MethodGet {
			return c.Redirect("/login", http.StatusFound)
		}
		return c.Status(http.StatusUnauthorized).SendString("Требуется авторизация")
	}
	// Cookie уходит на любой запрос с нашего сайта, поэтому изменяющие
	// действия дополнительно сверяются с источником: POST с чужого домена
	// отбрасывается, даже если браузер приложил сессию.
	if !sameOrigin(c) {
		return c.Status(http.StatusForbidden).SendString("Запрос с чужого источника отклонён")
	}
	c.Locals("claims", claims)
	return c.Next()
}

// sameOrigin проверяет Origin (или Referer, если Origin не передан) для
// небезопасных методов. Отсутствие обоих заголовков допускается: так ведут
// себя curl и серверные клиенты, для которых CSRF неприменим.
func sameOrigin(c *fiber.Ctx) bool {
	switch c.Method() {
	case fiber.MethodGet, fiber.MethodHead, fiber.MethodOptions:
		return true
	}

	origin := c.Get(fiber.HeaderOrigin)
	if origin == "" {
		origin = c.Get(fiber.HeaderReferer)
	}
	if origin == "" {
		return true
	}

	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	// Сравниваем без порта: за обратным прокси порт снаружи и внутри может
	// отличаться, а имя хоста — нет.
	return strings.EqualFold(hostOnly(u.Host), hostOnly(c.Hostname()))
}

func hostOnly(hostport string) string {
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		return host
	}
	return hostport
}

// requireAdmin защищает административный раздел панели.
func (h *Handler) requireAdmin(c *fiber.Ctx) error {
	if !h.currentUser(c).IsAdmin() {
		return c.Status(http.StatusForbidden).SendString("Доступ только для администратора")
	}
	return c.Next()
}

func (h *Handler) currentUser(c *fiber.Ctx) *session.Claims {
	claims, _ := c.Locals("claims").(*session.Claims)
	if claims == nil {
		return &session.Claims{}
	}
	return claims
}

// ── Страницы ──────────────────────────────────────────────────────────────

// LoginPage GET /login
func (h *Handler) LoginPage(c *fiber.Ctx) error {
	if h.session(c) != nil {
		return c.Redirect("/", http.StatusFound)
	}
	return c.Render("login", fiber.Map{"Title": "Вход"})
}

// LoginSubmit POST /login
func (h *Handler) LoginSubmit(c *fiber.Ctx) error {
	if !sameOrigin(c) {
		return c.Status(http.StatusForbidden).SendString("Запрос с чужого источника отклонён")
	}

	login := strings.TrimSpace(c.FormValue("login"))

	client := c.IP()
	if h.limiter != nil && !h.limiter.Allowed(client) {
		return c.Status(http.StatusTooManyRequests).Render("login", fiber.Map{
			"Title": "Вход",
			"Error": "Слишком много попыток входа. Попробуйте позже.",
			"Login": login,
		})
	}

	u, err := h.svc.Authenticate(login, c.FormValue("password"))
	if err != nil {
		if h.limiter != nil {
			h.limiter.Fail(client)
		}
		return c.Status(http.StatusUnauthorized).Render("login", fiber.Map{
			"Title": "Вход",
			"Error": "Неверный логин или пароль",
			"Login": login,
		})
	}
	if h.limiter != nil {
		h.limiter.Reset(client)
	}

	token, err := h.sessions.Issue(u)
	if err != nil {
		return c.Status(http.StatusInternalServerError).SendString("Не удалось создать сессию")
	}
	c.Cookie(&fiber.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HTTPOnly: true,
		SameSite: fiber.CookieSameSiteLaxMode,
		Expires:  time.Now().Add(h.sessions.TTL()),
	})
	return c.Redirect("/", http.StatusFound)
}

// BindTelegramChat POST /users/:id/chat
func (h *Handler) BindTelegramChat(c *fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	chatID := strings.TrimSpace(c.FormValue("tg_chat_id"))
	if err := h.svc.SetUserChat(id, chatID); err != nil {
		return h.badRequest(c, err)
	}
	return usersRedirect(c, "Telegram чат успешно привязан")
}

// Logout POST /logout
func (h *Handler) Logout(c *fiber.Ctx) error {
	if !sameOrigin(c) {
		return c.Status(http.StatusForbidden).SendString("Запрос с чужого источника отклонён")
	}
	if claims := h.session(c); claims != nil && claims.ExpiresAt != nil {
		_ = h.svc.RevokeSession(claims.ID, claims.ExpiresAt.Time)
	}
	c.ClearCookie(sessionCookie)
	return c.Redirect("/login", http.StatusFound)
}

// ── Представления ─────────────────────────────────────────────────────────

// occurrenceView — строка таблицы периодов. Готовится здесь, чтобы шаблон не
// занимался арифметикой дат.
type occurrenceView struct {
	ID          int64
	PaymentID   int64
	PaymentName string
	Amount      string
	RawAmount   float64
	UserID      int64
	UserName    string
	DueDate     string
	PaidAt      string
	Status      string
	StatusLabel string
	Overdue     string
	IsPaid      bool
}

var statusLabels = map[domain.Status]string{
	domain.StatusPlanned: "Ожидает",
	domain.StatusDue:     "Срок сегодня",
	domain.StatusOverdue: "Просрочено",
	domain.StatusPaid:    "Оплачено",
}

func (h *Handler) views(list []domain.Occurrence) []occurrenceView {
	today := h.svc.Today()
	out := make([]occurrenceView, 0, len(list))
	for _, o := range list {
		v := occurrenceView{
			ID:          o.ID,
			PaymentID:   o.PaymentID,
			PaymentName: o.PaymentName,
			Amount:      fmt.Sprintf("%s %.2f", o.Currency, o.Amount.Major()),
			RawAmount:   o.Amount.Major(),
			UserID:      o.UserID,
			UserName:    o.UserName,
			DueDate:     o.DueDate.String(),
			Status:      string(o.Status),
			StatusLabel: statusLabels[o.Status],
			IsPaid:      o.PaidAt != nil,
		}
		if o.PaidAt != nil {
			v.PaidAt = o.PaidAt.String()
		}
		if o.Status == domain.StatusOverdue {
			v.Overdue = fmt.Sprintf("%d дн.", o.DueDate.DaysUntil(today))
		}
		out = append(out, v)
	}
	return out
}

// userView — строка таблицы пользователей со счётчиком закреплённых платежей.
type userView struct {
	*domain.User
	Payments int
}

func (h *Handler) userViews() ([]userView, error) {
	users, err := h.svc.Users()
	if err != nil {
		return nil, err
	}
	out := make([]userView, 0, len(users))
	for i := range users {

		if users[i].IsAdmin() {
			continue
		}

		payments, err := h.svc.PaymentsFiltered(false, users[i].ID)
		if err != nil {
			return nil, err
		}

		out = append(out, userView{User: &users[i], Payments: len(payments)})
	}
	return out, nil
}

// pageData — общие поля любого рендера: кто смотрит, какая вкладка активна и
// есть ли сообщение об ошибке.
func (h *Handler) pageData(c *fiber.Ctx, title, active string, extra fiber.Map) fiber.Map {
	user := h.currentUser(c)
	data := fiber.Map{
		"Title":   title,
		"Active":  active,
		"User":    user,
		"IsAdmin": user.IsAdmin(),
		"Error":   c.Query("error"),
		"Notice":  c.Query("notice"),
		"Today":   h.svc.Today().String(),
	}
	for k, v := range extra {
		data[k] = v
	}
	return data
}

// parseOwnerTranslate разбирает значение фильтра «Ответственный».
func parseOwner(raw string) (int64, string) {
	switch raw {
	case "":
		return filterAll, ownerDefault
	case ownerNone:
		return filterNone, ownerNone
	}
	if id, err := strconv.ParseInt(raw, 10, 64); err == nil && id > 0 {
		return id, raw
	}
	return filterAll, ownerDefault
}

// ownerParam — значение для скрытых полей форм: фильтр сохраняется после
// редиректа.
func ownerParam(raw string) string {
	if raw == "" || raw == ownerAll {
		return ""
	}
	return raw
}

// dashboardRedirect — возврат на дашборд с сохранением фильтра.
func dashboardRedirect(c *fiber.Ctx, query string) error {
	if query == "" {
		return c.Redirect("/", http.StatusFound)
	}
	return c.Redirect("/?owner="+url.QueryEscape(query), http.StatusFound)
}

// Dashboard GET /
func (h *Handler) Dashboard(c *fiber.Ctx) error {
	filter, owner := parseOwner(c.Query("owner"))

	sortParam := c.Query("sort", "date_asc")
	rangeParam := c.Query("range", "all")

	occurrences, err := h.occurrences(filter)
	if err != nil {
		return c.Status(http.StatusInternalServerError).SendString("Не удалось загрузить платежи")
	}
	summary, err := h.summary(filter)
	if err != nil {
		return c.Status(http.StatusInternalServerError).SendString("Не удалось посчитать итоги")
	}
	users, err := h.userViews()
	if err != nil {
		return c.Status(http.StatusInternalServerError).SendString("Не удалось загрузить пользователей")
	}

	// Превращаем в представления (views)
	rows := h.views(occurrences)

	today := h.svc.Today()
	if rangeParam != "all" {
		var monthsToAdd int
		switch rangeParam {
		case "1m":
			monthsToAdd = 1
		case "3m":
			monthsToAdd = 3
		case "6m":
			monthsToAdd = 6
		}
		limitDate := today.AddMonths(monthsToAdd)

		var filteredRows []occurrenceView
		for _, r := range rows {
			// Парсим дату обратно или сравниваем строки (формат YYYY-MM-DD позволяет лексикографическое сравнение!)
			// Рассматриваем периоды, которые наступают не позже limitDate
			if r.DueDate <= limitDate.String() {
				filteredRows = append(filteredRows, r)
			}
		}
		rows = filteredRows
	}

	switch sortParam {
	case "date_asc":
		sort.Slice(rows, func(i, j int) bool {
			return rows[i].DueDate < rows[j].DueDate
		})
	case "date_desc":
		sort.Slice(rows, func(i, j int) bool {
			return rows[i].DueDate > rows[j].DueDate
		})
	case "amount_desc":
		sort.Slice(rows, func(i, j int) bool {
			return rows[i].RawAmount > rows[j].RawAmount
		})
	}

	// 3. Передаем "Sort" в fiber.Map, чтобы шаблон знал текущую сортировку
	return c.Render("dashboard", h.pageData(c, "Платежи", "dashboard", fiber.Map{
		"Rows":       rows,
		"Summary":    summary,
		"Users":      users,
		"Owner":      owner,
		"Sort":       sortParam, // <--- ЭТОТ ПАРАМЕТР ВАЖЕН ДЛЯ СЕЛИКТОРА В HTML
		"Range":      rangeParam,
		"OwnerQuery": ownerParam(c.Query("owner")),
		"Currencies": domain.Currencies,
		"Periods":    domain.Periods,
	}))
}

func (h *Handler) occurrences(filter int64) ([]domain.Occurrence, error) {
	if filter == filterAll {
		return h.svc.Occurrences(0)
	}
	return h.svc.OccurrencesFiltered(0, filter)
}

func (h *Handler) summary(filter int64) (*app.Totals, error) {
	if filter == filterAll {
		return h.svc.Summary()
	}
	return h.svc.SummaryAssigned(filter)
}

// MarkPaid POST /occurrences/:id/paid
//
// Одна форма обслуживает три действия кейса: отметить оплату, исправить
// фактическую дату и снять отметку — всё через значение поля paid_at.
func (h *Handler) MarkPaid(c *fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	if _, err := h.svc.SetPaid(id, strings.TrimSpace(c.FormValue("paid_at"))); err != nil {
		return h.badRequest(c, err)
	}
	return dashboardRedirect(c, c.FormValue("owner"))
}

// SetOccurrenceDue POST /occurrences/:id/due
//
// Разовый перенос срока периода: шаблон и остальные периоды не меняются.
func (h *Handler) SetOccurrenceDue(c *fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	if _, err := h.svc.SetOccurrenceDue(id, strings.TrimSpace(c.FormValue("due_date"))); err != nil {
		return h.badRequest(c, err)
	}
	return dashboardRedirect(c, c.FormValue("owner"))
}

// AssignOccurrenceOwner POST /occurrences/:id/owner
//
// Ответственный принадлежит шаблону, поэтому назначение из строки периода
// меняет весь платёж — иначе периоды одного платежа разошлись бы по людям.
func (h *Handler) AssignOccurrenceOwner(c *fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	occurrence, err := h.svc.Occurrence(id)
	if err != nil {
		return h.failErr(c, err)
	}
	userID, err := formInt(c, "user_id")
	if err != nil {
		return h.badRequest(c, err)
	}
	if _, err := h.svc.AssignPaymentUser(occurrence.PaymentID, userID); err != nil {
		return h.failErr(c, err)
	}
	return dashboardRedirect(c, c.FormValue("owner"))
}

// ── Шаблоны платежей ──────────────────────────────────────────────────────

// PaymentsPage GET /payments
func (h *Handler) PaymentsPage(c *fiber.Ctx) error {
	payments, err := h.svc.PaymentsFiltered(false, filterAll)
	if err != nil {
		return c.Status(http.StatusInternalServerError).SendString("Не удалось загрузить шаблоны")
	}
	users, err := h.userViews()
	if err != nil {
		return c.Status(http.StatusInternalServerError).SendString("Не удалось загрузить пользователей")
	}

	return c.Render("payments", h.pageData(c, "Шаблоны", "payments", fiber.Map{
		"Payments":   payments,
		"Users":      users,
		"Currencies": domain.Currencies,
		"Periods":    domain.Periods,
	}))
}

// PaymentEditPage GET /payments/:id/edit
func (h *Handler) PaymentEditPage(c *fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	payment, err := h.svc.Payment(id)
	if err != nil {
		return h.failErr(c, err)
	}
	occurrences, err := h.svc.OccurrencesFiltered(id, filterAll)
	if err != nil {
		return c.Status(http.StatusInternalServerError).SendString("Не удалось загрузить периоды")
	}
	users, err := h.userViews()
	if err != nil {
		return c.Status(http.StatusInternalServerError).SendString("Не удалось загрузить пользователей")
	}

	return c.Render("payment-edit", h.pageData(c, "Шаблоны", "payments", fiber.Map{
		"Payment":    payment,
		"Rows":       h.views(occurrences),
		"Users":      users,
		"Currencies": domain.Currencies,
		"Periods":    domain.Periods,
	}))
}

// CreatePayment POST /payments
func (h *Handler) CreatePayment(c *fiber.Ctx) error {
	payment, err := h.svc.CreatePayment(paymentForm(c))
	if err != nil {
		return h.badRequest(c, err)
	}
	return c.Redirect("/payments/"+strconv.FormatInt(payment.ID, 10)+"/edit", http.StatusFound)
}

func (h *Handler) DeletePayment(c *fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	if err := h.svc.DeletePayment(id); err != nil {
		return h.badRequest(c, err)
	}
	return paymentsRedirect(c, "Платёж полностью удалён")
}

// UpdatePayment POST /payments/:id
func (h *Handler) UpdatePayment(c *fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	if _, err := h.svc.UpdatePayment(id, paymentForm(c)); err != nil {
		return h.badRequest(c, err)
	}
	return c.Redirect("/payments/"+strconv.FormatInt(id, 10)+"/edit?notice="+
		url.QueryEscape("Изменения сохранены, будущие периоды пересчитаны"), http.StatusFound)
}

// AssignPaymentOwner POST /payments/:id/owner
func (h *Handler) AssignPaymentOwner(c *fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	userID, err := formInt(c, "user_id")
	if err != nil {
		return h.badRequest(c, err)
	}
	if _, err := h.svc.AssignPaymentUser(id, userID); err != nil {
		return h.failErr(c, err)
	}
	return paymentsRedirect(c, "Ответственный обновлён — периоды унаследовали его")
}

// StopPayment POST /payments/:id/stop
func (h *Handler) StopPayment(c *fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	if err := h.svc.StopPayment(id); err != nil {
		return h.failErr(c, err)
	}
	return paymentsRedirect(c, "Платёж остановлен, история оплат сохранена")
}

// ResumePayment POST /payments/:id/resume
func (h *Handler) ResumePayment(c *fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	if _, err := h.svc.ResumePayment(id); err != nil {
		return h.failErr(c, err)
	}
	return paymentsRedirect(c, "Платёж возобновлён")
}

func paymentsRedirect(c *fiber.Ctx, notice string) error {
	if notice == "" {
		return c.Redirect("/payments", http.StatusFound)
	}
	return c.Redirect("/payments?notice="+url.QueryEscape(notice), http.StatusFound)
}

// paymentForm разбирает форму создания/изменения шаблона.
func paymentForm(c *fiber.Ctx) app.PaymentInput {
	amount, _ := strconv.ParseFloat(strings.ReplaceAll(c.FormValue("amount"), ",", "."), 64)
	userID, _ := strconv.ParseInt(c.FormValue("user_id"), 10, 64)
	return app.PaymentInput{
		Name:     strings.TrimSpace(c.FormValue("name")),
		Amount:   amount,
		Currency: domain.Currency(c.FormValue("currency")),
		Period:   domain.Period(c.FormValue("period")),
		DueDate:  c.FormValue("due_date"),
		UserID:   userID,
	}
}

// ── Пользователи ──────────────────────────────────────────────────────────

// UsersPage GET /users
func (h *Handler) UsersPage(c *fiber.Ctx) error {
	users, err := h.userViews()
	if err != nil {
		return c.Status(http.StatusInternalServerError).SendString("Не удалось загрузить пользователей")
	}
	return c.Render("users", h.pageData(c, "Пользователи", "users", fiber.Map{"Users": users}))
}

// CreateUser POST /users
func (h *Handler) CreateUser(c *fiber.Ctx) error {
	login := strings.TrimSpace(c.FormValue("login"))
	if _, err := h.svc.CreateUser(login); err != nil {
		return h.badRequest(c, err)
	}
	return usersRedirect(c, "Пользователь «"+login+"» создан")
}

// UpdateUser POST /users/:id
func (h *Handler) UpdateUser(c *fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	updated, err := h.svc.UpdateUser(id, strings.TrimSpace(c.FormValue("login")))
	if err != nil {
		return h.badRequest(c, err)
	}
	return usersRedirect(c, "Данные «"+updated.Login+"» обновлены")
}

// DeleteUser POST /users/:id/delete
func (h *Handler) DeleteUser(c *fiber.Ctx) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	if err := h.svc.DeleteUser(id); err != nil {
		return h.failErr(c, err)
	}
	return usersRedirect(c, "Пользователь удалён")
}

func usersRedirect(c *fiber.Ctx, notice string) error {
	if notice == "" {
		return c.Redirect("/users", http.StatusFound)
	}
	return c.Redirect("/users?notice="+url.QueryEscape(notice), http.StatusFound)
}

// ── Ошибки ────────────────────────────────────────────────────────────────

func idParam(c *fiber.Ctx) (int64, error) {
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, fiber.NewError(http.StatusBadRequest, "Некорректный идентификатор")
	}
	return id, nil
}

// formInt читает числовое поле формы; пустое значение — 0.
func formInt(c *fiber.Ctx, name string) (int64, error) {
	raw := strings.TrimSpace(c.FormValue(name))
	if raw == "" {
		return 0, nil
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 0 {
		return 0, domain.Invalid("некорректное значение поля %s", name)
	}
	return id, nil
}

// badRequest отдаёт понятную текст ошибки вместо 500: панель отправляет те же
// данные, что и API, поэтому доменные проверки здесь повторяются в том же виде.
func (h *Handler) badRequest(c *fiber.Ctx, err error) error {
	msg := err.Error()
	if domain.IsNotFound(err) {
		return c.Status(http.StatusNotFound).SendString("Запись не найдена")
	}
	return c.Status(http.StatusBadRequest).SendString(msg)
}

// failErr переводит доменную ошибку в код панели: отсутствующая запись — 404,
// остальное — 400 с текстом проверки.
func (h *Handler) failErr(c *fiber.Ctx, err error) error {
	if domain.IsNotFound(err) {
		return c.Status(http.StatusNotFound).SendString("Запись не найдена")
	}
	return c.Status(http.StatusBadRequest).SendString(err.Error())
}
