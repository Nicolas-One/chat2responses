package main

import (
	_ "embed"
	"net/http"
)

//go:embed admin.html
var adminHTML string

// adminPageHandler 返回管理后台 HTML 页面（通过 //go:embed 从 admin.html 编译嵌入）。
func adminPageHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(adminHTML))
}
