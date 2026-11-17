package main

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
)

const addr = ":8080"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck(addr))
	}
	apiURL, err := url.Parse(env("API_URL", "http://api:8080"))
	if err != nil {
		panic(err)
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(apiURL)
			r.Out.Header.Del("Cookie") // ブラウザの Cookie は API に渡さない
			r.SetXForwarded()
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /bff/whoami", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"served_by": hostname, "remote_addr": r.RemoteAddr, "x_forwarded_for": r.Header.Get("X-Forwarded-For")})
	})
	mux.Handle("/api/", proxy)

	serve(addr, mux)
}
