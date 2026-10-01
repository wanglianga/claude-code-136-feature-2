package main

import (
	"context"
	"embed"
	"log"
	"os"
	"strings"

	"github.com/gofiber/fiber/v2"
	"passgate/internal/handler"
	"passgate/internal/seed"
	"passgate/internal/service"
	"passgate/internal/store"
)

//go:embed static
var staticFS embed.FS

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func contentType(rel string) string {
	switch {
	case strings.HasSuffix(rel, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(rel, ".js"):
		return "application/javascript; charset=utf-8"
	case strings.HasSuffix(rel, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(rel, ".yaml") || strings.HasSuffix(rel, ".yml"):
		return "application/yaml; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

func main() {
	redisAddr := getenv("REDIS_ADDR", "127.0.0.1:6379")
	st := store.New(redisAddr, "", 0)
	ctx := context.Background()
	if err := st.Ping(ctx); err != nil {
		log.Fatalf("无法连接 Redis(%s): %v", redisAddr, err)
	}
	svc := service.New(st)
	if err := seed.SeedIfEmpty(ctx, svc); err != nil {
		log.Printf("[seed] 失败: %v", err)
	}

	app := fiber.New(fiber.Config{
		AppName:      "passgate 景区年卡核验服务",
		ErrorHandler: handler.ErrorHandler,
	})
	handler.Register(app, svc)

	// OpenAPI 3.x 文档 + Swagger UI（资源随二进制内嵌，部署不依赖工作目录）
	spec, err := staticFS.ReadFile("static/openapi.yaml")
	if err != nil {
		log.Fatalf("openapi 文档缺失: %v", err)
	}
	app.Get("/openapi.yaml", func(c *fiber.Ctx) error {
		c.Set("Content-Type", "application/yaml; charset=utf-8")
		return c.Send(spec)
	})
	app.Get("/docs", func(c *fiber.Ctx) error {
		return c.Redirect("/static/swagger-ui/index.html")
	})
	app.Get("/static/*", func(c *fiber.Ctx) error {
		rel := c.Params("*")
		data, err := staticFS.ReadFile("static/" + strings.TrimPrefix(rel, "/"))
		if err != nil {
			return c.Status(fiber.StatusNotFound).SendString("not found")
		}
		c.Set("Content-Type", contentType(rel))
		return c.Send(data)
	})

	port := getenv("PORT", "8080")
	log.Printf("listening on :%s (redis=%s)", port, redisAddr)
	log.Printf("Swagger UI: http://127.0.0.1:%s/docs", port)
	log.Fatal(app.Listen(":" + port))
}
