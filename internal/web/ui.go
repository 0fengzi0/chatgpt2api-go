package web

import (
	_ "embed"
	"net/http"
)

//go:embed index.html
var index []byte

func Serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(index)
}
