package main

import (
	"net/http"
)

// Healthz handles GET /healthz — always 200.
func (a *App) Healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

// Readyz handles GET /readyz — 200 when DB is reachable.
func (a *App) Readyz(w http.ResponseWriter, r *http.Request) {
	if err := a.DB.HealthCheck(r.Context()); err != nil {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}
