// Спецификация встроена в бинарник: Swagger UI отдаёт ровно тот файл,который лежит в репозитории, и приложение не зависит от рабочего каталога.
package docs

import _ "embed"

//go:embed openapi.yml
var OpenAPI []byte
