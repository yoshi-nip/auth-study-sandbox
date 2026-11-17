package main

import (
	"net/http"
	"os"
)

const addr = ":8080"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck(addr))
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// 認証なしで呼べる公開エンドポイント（ルーティングの確認用）
	mux.HandleFunc("GET /api/public/hello", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"message": "hello from api", "served_by": hostname})
	})

	serve(addr, mux)
}
