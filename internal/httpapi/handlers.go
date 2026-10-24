// Package httpapi — REST API Invoice Keeper (Fiber v2).
//
// Слой отвечает только за транспорт: разбор запроса, аутентификацию, маппинг
// доменных ошибок в HTTP-коды и сериализацию. Бизнес-правил здесь нет.
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/aeza/invoice-keeper/internal/app"
	"github.com/aeza/invoice-keeper/internal/domain"
	"github.com/aeza/invoice-keeper/internal/ratelimit"
	"github.com/aeza/invoice-keeper/internal/session"
)

// ── Контекст и guard-ы ────────────────────────────────────────────────────

type ctxKey string

const ctxClaims ctxKey = "claims"

// requireAuth проверяет Bearer-токен, отзыв сессии и кладёт claims в контекст.
//
// Роль и существование учётной записи берутся из базы, а не из токена: иначе
// удалённый пользователь сохранял бы доступ до истечения токена, а понижённый
// администратор — права до следующего входа.
func (h *Handler) requireAuth(c *fiber.Ctx) error {
	header := c.Get(fiber.HeaderAuthorization)
	raw, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || raw == "" {
		return h.fail(c, http.StatusUnauthorized, "требуется авторизация")
	}

	claims, err := h.sessions.Verify(raw)
	if err != nil {
		return h.fail(c, http.StatusUnauthorized, "недействительный токен")
	}
	revoked, err := h.svc.SessionRevoked(claims.ID)
	if err != nil {
		return h.fail(c, http.StatusInternalServerError, "ошибка проверки сессии")
	}
	if revoked {
		return h.fail(c, http.StatusUnauthorized, "сессия завершена")
	}

	user, err := h.svc.User(claims.UserID)
	if domain.IsNotFound(err) {
		return h.fail(c, http.StatusUnauthorized, "учётная запись не найдена")
	}
	if err != nil {
		return h.fail(c, http.StatusInternalServerError, "ошибка проверки учётной записи")
	}

	claims.Login, claims.Role = user.Login, string(user.Role)
	c.Locals(ctxClaims, claims)
	return c.Next()
}

// requireAdmin отсекает обычных пользователей. Работает после requireAuth и
// применяется к административным операциям, поэтому ограничение действует
// одинаково и в панели, и в API.
func (h *Handler) requireAdmin(c *fiber.Ctx) error {
	if !currentClaims(c).IsAdmin() {
		return h.fail(c, http.StatusForbidden, "требуются права администратора")
	}
	return c.Next()
}

func currentClaims(c *fiber.Ctx) *session.Claims {
	claims, _ := c.Locals(ctxClaims).(*session.Claims)
	if claims == nil {
		return &session.Claims{}
	}
	return claims
}

// ── Handler ───────────────────────────────────────────────────────────────

// Handler — обработчики API и общие помощники ответов.
type Handler struct {
	svc      *app.App
	sessions *session.Service
	limiter  *ratelimit.Limiter
}

// New создаёт обработчики. limiter может быть nil — тогда ограничение
// попыток входа отключено (например, в изолированных тестах).
func New(svc *app.App, sessions *session.Service, limiter *ratelimit.Limiter) *Handler {
	return &Handler{svc: svc, sessions: sessions, limiter: limiter}
}

// Register развешивает маршруты API. Префикс /api/v1 задаёт вызывающая сторона.
//
// requireAuth указывается в каждом маршруте явно, а не через Group(""):
// middleware группы в Fiber применяется ко всему префиксу, из-за чего под
// авторизацию попали бы и публичные /docs.
func (h *Handler) Register(api fiber.Router) {
	api.Post("/auth/login", h.Login)
	api.Get("/auth/me", h.requireAuth, h.Me)
	api.Post("/auth/logout", h.requireAuth, h.Logout)

	api.Get("/recurring-payments", h.requireAuth, h.ListPayments)
	api.Get("/recurring-payments/:id", h.requireAuth, h.GetPayment)
	api.Post("/recurring-payments", h.requireAuth, h.requireAdmin, h.CreatePayment)
	api.Patch("/recurring-payments/:id", h.requireAuth, h.requireAdmin, h.UpdatePayment)
	api.Put("/recurring-payments/:id/assign", h.requireAuth, h.requireAdmin, h.AssignPayment)
	api.Post("/recurring-payments/:id/resume", h.requireAuth, h.requireAdmin, h.ResumePayment)
	api.Delete("/recurring-payments/:id", h.requireAuth, h.requireAdmin, h.StopPayment)

	api.Get("/payment-occurrences", h.requireAuth, h.ListOccurrences)
	api.Get("/payment-occurrences/:id", h.requireAuth, h.GetOccurrence)
	// Отметку об оплате и фактическую дату меняет любой пользователь: это его
	// право по условию кейса, а не привилегия администратора.
	api.Patch("/payment-occurrences/:id", h.requireAuth, h.UpdateOccurrence)

	api.Get("/payments/summary", h.requireAuth, h.Summary)

	api.Get("/users", h.requireAuth, h.requireAdmin, h.ListUsers)
	api.Post("/users", h.requireAuth, h.requireAdmin, h.CreateUser)
	api.Patch("/users/:id", h.requireAuth, h.requireAdmin, h.UpdateUser)
	api.Delete("/users/:id", h.requireAuth, h.requireAdmin, h.DeleteUser)
}

// ── Ответы и ошибки ───────────────────────────────────────────────────────

// fail отдаёт ошибку в едином формате {"error": "..."}.
func (h *Handler) fail(c *fiber.Ctx, status int, msg string) error {
	return c.Status(status).JSON(fiber.Map{"error": msg})
}

// failErr переводит доменную ошибку в HTTP-код: 404 для отсутствующей записи,
// 400 для нарушенной валидации, 409 для занятого логина, 500 для остального.
// Такой маппинг избавляет обработчики от ручных проверок на каждом шаге.
func (h *Handler) failErr(c *fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return h.fail(c, http.StatusNotFound, "запись не найдена")
	case errors.Is(err, domain.ErrInvalidInput):
		return h.fail(c, http.StatusBadRequest, strings.TrimPrefix(err.Error(), domain.ErrInvalidInput.Error()+": "))
	case errors.Is(err, domain.ErrDuplicateLogin):
		return h.fail(c, http.StatusConflict, "логин уже занят")
	default:
		return h.fail(c, http.StatusInternalServerError, "внутренняя ошибка")
	}
}

// pathID разбирает :id и отвечает 400 при нечисловом значении.
func (h *Handler) pathID(c *fiber.Ctx) (int64, bool) {
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil || id <= 0 {
		_ = h.fail(c, http.StatusBadRequest, "некорректный идентификатор")
		return 0, false
	}
	return id, true
}

// ── Auth ──────────────────────────────────────────────────────────────────

type loginRequest struct {
	Login    string `json:"login"`
	Password string `json:"password"`
}

// Login POST /api/v1/auth/login
func (h *Handler) Login(c *fiber.Ctx) error {
	var req loginRequest
	if err := c.BodyParser(&req); err != nil {
		return h.fail(c, http.StatusBadRequest, "некорректное тело запроса")
	}
	if req.Login == "" || req.Password == "" {
		return h.fail(c, http.StatusBadRequest, "логин и пароль обязательны")
	}

	// Перебор пароля — единственное, что может делать неавторизованный
	// клиент, поэтому попытки ограничены по адресу.
	client := c.IP()
	if h.limiter != nil && !h.limiter.Allowed(client) {
		c.Set(fiber.HeaderRetryAfter, strconv.Itoa(int(h.limiter.RetryAfter(client).Seconds())+1))
		return h.fail(c, http.StatusTooManyRequests, "слишком много попыток входа, попробуйте позже")
	}

	u, err := h.svc.Authenticate(req.Login, req.Password)
	if err != nil {
		if h.limiter != nil {
			h.limiter.Fail(client)
		}
		// Наружу не сообщаем, существует ли логин.
		return h.fail(c, http.StatusUnauthorized, "неверный логин или пароль")
	}
	if h.limiter != nil {
		h.limiter.Reset(client)
	}

	token, err := h.sessions.Issue(u)
	if err != nil {
		return h.fail(c, http.StatusInternalServerError, "не удалось выдать токен")
	}
	return c.JSON(fiber.Map{"token": token, "user": u})
}

// Me GET /api/v1/auth/me
func (h *Handler) Me(c *fiber.Ctx) error {
	u, err := h.svc.User(currentClaims(c).UserID)
	if errors.Is(err, domain.ErrNotFound) {
		// Токен валиден, но учётную запись удалили — сессия больше не действует.
		return h.fail(c, http.StatusUnauthorized, "учётная запись не найдена")
	}
	if err != nil {
		return h.failErr(c, err)
	}
	return c.JSON(u)
}

// Logout POST /api/v1/auth/logout
func (h *Handler) Logout(c *fiber.Ctx) error {
	claims := currentClaims(c)
	if claims.ID != "" && claims.ExpiresAt != nil {
		if err := h.svc.RevokeSession(claims.ID, claims.ExpiresAt.Time); err != nil {
			return h.failErr(c, err)
		}
	}
	return c.JSON(fiber.Map{"message": "сессия завершена"})
}

// ── Регулярные платежи ────────────────────────────────────────────────────

// ListPayments GET /api/v1/recurring-payments
//
// user_id — фильтр по ответственному; unassigned=1 оставляет платежи без
// ответственного; all=1 возвращает и остановленные.
func (h *Handler) ListPayments(c *fiber.Ctx) error {
	userID, unassigned, err := h.userFilter(c)
	if err != nil {
		return h.fail(c, http.StatusBadRequest, err.Error())
	}

	payments, err := h.svc.PaymentsFiltered(c.Query("all") != "1", userIDOr(unassigned, userID))
	if err != nil {
		return h.failErr(c, err)
	}
	if payments == nil {
		payments = []domain.RecurringPayment{}
	}
	return c.JSON(payments)
}

// userFilter разбирает пару user_id / unassigned.
func (h *Handler) userFilter(c *fiber.Ctx) (int64, bool, error) {
	var userID int64
	if raw := c.Query("user_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			return 0, false, errors.New("некорректный user_id")
		}
		userID = id
	}
	return userID, c.Query("unassigned") == "1", nil
}

// userIDOr сворачивает фильтр «без ответственного» в 0 — именно так он
// трактуется в хранилище.
func userIDOr(unassigned bool, userID int64) int64 {
	if unassigned {
		return 0
	}
	if userID == 0 {
		return app.NoUserFilter
	}
	return userID
}

// GetPayment GET /api/v1/recurring-payments/{id}
func (h *Handler) GetPayment(c *fiber.Ctx) error {
	id, ok := h.pathID(c)
	if !ok {
		return nil
	}
	p, err := h.svc.Payment(id)
	if err != nil {
		return h.failErr(c, err)
	}
	return c.JSON(p)
}

// CreatePayment POST /api/v1/recurring-payments
func (h *Handler) CreatePayment(c *fiber.Ctx) error {
	var req app.PaymentInput
	if err := c.BodyParser(&req); err != nil {
		return h.fail(c, http.StatusBadRequest, "некорректное тело запроса")
	}
	p, err := h.svc.CreatePayment(req)
	if err != nil {
		return h.failErr(c, err)
	}
	return c.Status(http.StatusCreated).JSON(p)
}

// UpdatePayment PATCH /api/v1/recurring-payments/{id}
//
// Меняются только переданные поля. Смена first_due_date или period
// пересобирает будущие периоды, оплаченные остаются нетронутыми.
func (h *Handler) UpdatePayment(c *fiber.Ctx) error {
	id, ok := h.pathID(c)
	if !ok {
		return nil
	}
	var req app.PaymentInput
	if err := c.BodyParser(&req); err != nil {
		return h.fail(c, http.StatusBadRequest, "некорректное тело запроса")
	}
	p, err := h.svc.UpdatePayment(id, req)
	if err != nil {
		return h.failErr(c, err)
	}
	return c.JSON(p)
}

type assignRequest struct {
	UserID *int64 `json:"user_id"`
}

// AssignPayment PUT /api/v1/recurring-payments/{id}/assign
//
// user_id = 0 снимает ответственного.
func (h *Handler) AssignPayment(c *fiber.Ctx) error {
	id, ok := h.pathID(c)
	if !ok {
		return nil
	}
	var req assignRequest
	if err := c.BodyParser(&req); err != nil {
		return h.fail(c, http.StatusBadRequest, "некорректное тело запроса")
	}
	if req.UserID == nil {
		return h.fail(c, http.StatusBadRequest, "поле user_id обязательно")
	}

	p, err := h.svc.AssignPaymentUser(id, *req.UserID)
	if err != nil {
		return h.failErr(c, err)
	}
	return c.JSON(p)
}

// ResumePayment POST /api/v1/recurring-payments/{id}/resume
func (h *Handler) ResumePayment(c *fiber.Ctx) error {
	id, ok := h.pathID(c)
	if !ok {
		return nil
	}
	p, err := h.svc.ResumePayment(id)
	if err != nil {
		return h.failErr(c, err)
	}
	return c.JSON(p)
}

// StopPayment DELETE /api/v1/recurring-payments/{id}
func (h *Handler) StopPayment(c *fiber.Ctx) error {
	id, ok := h.pathID(c)
	if !ok {
		return nil
	}
	if err := h.svc.StopPayment(id); err != nil {
		return h.failErr(c, err)
	}
	return c.JSON(fiber.Map{"message": "платёж прекращён, история сохранена"})
}

// ── Периоды оплаты ────────────────────────────────────────────────────────

// ListOccurrences GET /api/v1/payment-occurrences
//
// payment_id фильтрует по шаблону, user_id / unassigned — по ответственному.
func (h *Handler) ListOccurrences(c *fiber.Ctx) error {
	var paymentID int64
	if raw := c.Query("payment_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			return h.fail(c, http.StatusBadRequest, "некорректный payment_id")
		}
		paymentID = id
	}
	userID, unassigned, err := h.userFilter(c)
	if err != nil {
		return h.fail(c, http.StatusBadRequest, err.Error())
	}

	list, err := h.svc.OccurrencesFiltered(paymentID, userIDOr(unassigned, userID))
	if err != nil {
		return h.failErr(c, err)
	}
	if list == nil {
		list = []domain.Occurrence{}
	}
	return c.JSON(list)
}

// GetOccurrence GET /api/v1/payment-occurrences/{id}
func (h *Handler) GetOccurrence(c *fiber.Ctx) error {
	id, ok := h.pathID(c)
	if !ok {
		return nil
	}
	o, err := h.svc.Occurrence(id)
	if err != nil {
		return h.failErr(c, err)
	}
	return c.JSON(o)
}

// updateOccurrenceRequest — тело PATCH. Отсутствие поля означает «не менять»,
// null или "" в paid_at — «снять отметку об оплате».
type updateOccurrenceRequest struct {
	PaidAt  *string
	DueDate *string
}

// parseOccurrencePatch различает отсутствие поля и явный null: для BodyParser
// они выглядят одинаково, а смысл разный — null снимает отметку об оплате.
func parseOccurrenceRequest(raw []byte) (updateOccurrenceRequest, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return updateOccurrenceRequest{}, err
	}

	var req updateOccurrenceRequest
	for key, target := range map[string]**string{"paid_at": &req.PaidAt, "due_date": &req.DueDate} {
		value, ok := fields[key]
		if !ok {
			continue
		}
		if string(value) == "null" {
			empty := ""
			*target = &empty
			continue
		}
		var s string
		if err := json.Unmarshal(value, &s); err != nil {
			return updateOccurrenceRequest{}, err
		}
		*target = &s
	}
	return req, nil
}

// UpdateOccurrence PATCH /api/v1/payment-occurrences/{id}
//
// Один эндпоинт закрывает операции кейса: подтвердить оплату
// ({"paid_at":"2026-11-01"}), исправить фактическую дату (другая дата),
// отменить оплату ({"paid_at":null}) и перенести срок периода
// ({"due_date":"2026-12-05"}).
func (h *Handler) UpdateOccurrence(c *fiber.Ctx) error {
	id, ok := h.pathID(c)
	if !ok {
		return nil
	}
	req, err := parseOccurrenceRequest(c.Body())
	if err != nil {
		return h.fail(c, http.StatusBadRequest, "некорректное тело запроса")
	}

	var o *domain.Occurrence
	switch {
	case req.DueDate != nil:
		o, err = h.svc.SetOccurrenceDue(id, *req.DueDate)
		if err == nil && req.PaidAt != nil {
			o, err = h.svc.SetPaid(id, *req.PaidAt)
		}
	case req.PaidAt != nil:
		o, err = h.svc.SetPaid(id, *req.PaidAt)
	default:
		o, err = h.svc.Occurrence(id)
	}
	if err != nil {
		return h.failErr(c, err)
	}
	return c.JSON(o)
}

// ── Итоги ─────────────────────────────────────────────────────────────────

// Summary GET /api/v1/payments/summary
//
// user_id / unassigned ограничивают итоги периодами конкретного
// ответственного.
func (h *Handler) Summary(c *fiber.Ctx) error {
	userID, unassigned, err := h.userFilter(c)
	if err != nil {
		return h.fail(c, http.StatusBadRequest, err.Error())
	}

	filter := userIDOr(unassigned, userID)
	totals, err := h.summaryFor(filter)
	if err != nil {
		return h.failErr(c, err)
	}
	return c.JSON(totals)
}

func (h *Handler) summaryFor(filter int64) (*app.Totals, error) {
	if filter == app.NoUserFilter {
		return h.svc.Summary()
	}
	return h.svc.SummaryAssigned(filter)
}

// ── Пользователи ──────────────────────────────────────────────────────────

type userRequest struct {
	Login    string `json:"login"`
	Password string `json:"password"`
}

// ListUsers GET /api/v1/users
func (h *Handler) ListUsers(c *fiber.Ctx) error {
	users, err := h.svc.Users()
	if err != nil {
		return h.failErr(c, err)
	}
	if users == nil {
		users = []domain.User{}
	}
	return c.JSON(users)
}

// CreateUser POST /api/v1/users
func (h *Handler) CreateUser(c *fiber.Ctx) error {
	var req userRequest
	if err := c.BodyParser(&req); err != nil {
		return h.fail(c, http.StatusBadRequest, "некорректное тело запроса")
	}
	u, err := h.svc.CreateUser(req.Login, req.Password)
	if err != nil {
		return h.failErr(c, err)
	}
	return c.Status(http.StatusCreated).JSON(u)
}

// UpdateUser PATCH /api/v1/users/{id}
func (h *Handler) UpdateUser(c *fiber.Ctx) error {
	id, ok := h.pathID(c)
	if !ok {
		return nil
	}
	var req userRequest
	if err := c.BodyParser(&req); err != nil {
		return h.fail(c, http.StatusBadRequest, "некорректное тело запроса")
	}
	u, err := h.svc.UpdateUser(id, req.Login, req.Password)
	if err != nil {
		return h.failErr(c, err)
	}
	return c.JSON(u)
}

// DeleteUser DELETE /api/v1/users/{id}
func (h *Handler) DeleteUser(c *fiber.Ctx) error {
	id, ok := h.pathID(c)
	if !ok {
		return nil
	}
	if err := h.svc.DeleteUser(id); err != nil {
		return h.failErr(c, err)
	}
	return c.JSON(fiber.Map{"message": "пользователь удалён"})
}
