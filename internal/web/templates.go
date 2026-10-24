package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/template/html/v2"

	"github.com/VechniyRabotnik/InvoiceKeeper-Hakaton/internal/domain"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Templates возвращает движок шаблонов, встроенных в бинарник.
//
// Без этого приложение зависело бы от рабочего каталога: запуск бинарника из
// любого места, кроме корня репозитория, отдавал бы 500 на всех страницах.
func Templates() *html.Engine {
	sub, err := fs.Sub(templatesFS, "templates")
	if err != nil {
		panic(err)
	}
	engine := html.NewFileSystem(http.FS(sub), ".html")
	engine.AddFunc("periodLabel", periodLabel)

	// ДОБАВЛЯЕМ МАТЕМАТИЧЕСКИЕ ФУНКЦИИ ДЛЯ ШАБЛОНОВ:
	engine.AddFunc("mul", func(a, b float64) float64 { return a * b })
	engine.AddFunc("div", func(a, b float64) float64 {
		if b == 0 {
			return 0
		}
		return a / b
	})

	return engine
}

// periodLabel — подписи периодичности в интерфейсе; в домене остаётся
// машинное значение, пригодное для API.
func periodLabel(p domain.Period) string {
	switch p {
	case domain.Monthly:
		return "Ежемесячно"
	case domain.Weekly:
		return "Еженедельно"
	case domain.Quarterly:
		return "Раз в квартал"
	case domain.HalfYear:
		return "Раз в полгода"
	case domain.Yearly:
		return "Ежегодно"
	default:
		return string(p)
	}
}

// Static раздаёт оформление панели. Файлы встроены в бинарник и меняются
// только вместе с ним, поэтому их можно кэшировать на стороне браузера.
func Static(r fiber.Router) {
	r.Get("/static/*", func(c *fiber.Ctx) error {
		name := strings.TrimPrefix(c.Path(), "/static/")
		body, err := staticFS.ReadFile("static/" + name)
		if err != nil {
			return fiber.ErrNotFound
		}
		c.Set(fiber.HeaderCacheControl, "public, max-age=300")
		if strings.HasSuffix(name, ".css") {
			c.Set(fiber.HeaderContentType, "text/css; charset=utf-8")
		}
		return c.Send(body)
	})
}
