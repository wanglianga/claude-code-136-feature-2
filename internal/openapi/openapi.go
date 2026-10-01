// Package openapi 内嵌 OpenAPI 3.x 规范与 Swagger UI 静态资源（离线可用）。
package openapi

import (
	"embed"
)

//go:embed openapi.yaml
var specYAML []byte

//go:embed assets/swagger-ui.css assets/swagger-ui-bundle.js
var assetsFS embed.FS

// SpecYAML 返回 OpenAPI 规范原文
func SpecYAML() []byte { return specYAML }

// Asset 读取内嵌静态资源（assets/ 前缀），不存在时返回 nil
func Asset(name string) []byte {
	b, err := assetsFS.ReadFile("assets/" + name)
	if err != nil {
		return nil
	}
	return b
}

// swaggerUIHTML 指向同源本地静态资源与 /openapi.yaml，无外网依赖。
const swaggerUIHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
  <meta charset="UTF-8" />
  <title>passgate API 文档 (Swagger UI)</title>
  <link rel="stylesheet" href="/docs/swagger-ui.css" />
  <style>body{margin:0} .topbar{display:none}</style>
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="/docs/swagger-ui-bundle.js"></script>
  <script>
    window.addEventListener('load', function () {
      window.ui = SwaggerUIBundle({
        url: '/openapi.yaml',
        dom_id: '#swagger-ui',
        deepLinking: true,
        presets: [SwaggerUIBundle.presets.apis],
        layout: 'BaseLayout'
      });
    });
  </script>
</body>
</html>`

// SwaggerUIHTML 返回 Swagger UI 页面 HTML
func SwaggerUIHTML() string { return swaggerUIHTML }
