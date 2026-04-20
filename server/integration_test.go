// Integration tests: spin up real Postgres + Redis containers via
// testcontainers-go and exercise the HTTP server through its chi
// router. Replaces the deleted Playwright e2e suite — same seams
// (JWT cookie, pgx pool, middleware wiring), fewer moving parts.
//
// Run with:
//
//	go test ./server/
//
// Requires a reachable Docker daemon. Without one, the whole
// package skips at TestMain (exit 0, no failure).

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cargocam/ghostcam/server/apitypes"
	"github.com/cargocam/ghostcam/server/auth"
	"github.com/cargocam/ghostcam/server/billing"
	"github.com/cargocam/ghostcam/server/db"
	"github.com/cargocam/ghostcam/server/linear"
	"github.com/cargocam/ghostcam/server/mailer"
	"github.com/cargocam/ghostcam/server/triage"
	"github.com/jackc/pgx/v5"
	goredis "github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

// testStack holds the shared container-backed dependencies. Containers
// are expensive to start (~5-10s each), so we start once in TestMain
// and share across every test in the package. Each test gets a fresh
// App wired to these shared connections and reset DB state.
type testStack struct {
	pgURL       string
	redisURL    string
	redisClient *goredis.Client
	cleanup     func()
}

var (
	stack     *testStack
	stackOnce sync.Once
	stackErr  error

	dockerOnce      sync.Once
	dockerAvailable bool
)

func probeDocker() bool {
	dockerOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		provider, err := testcontainers.NewDockerProvider()
		if err != nil {
			return
		}
		defer provider.Close()
		if err := provider.Health(ctx); err != nil {
			return
		}
		dockerAvailable = true
	})
	return dockerAvailable
}

// getStack lazily boots Postgres + Redis containers. Called by each
// test; the sync.Once ensures we pay the bring-up cost at most once.
// Tests skip themselves when Docker isn't reachable so `go test ./...`
// is still useful on dev machines without a running daemon.
func getStack(t *testing.T) *testStack {
	t.Helper()
	if !probeDocker() {
		t.Skip("docker daemon not reachable; skipping integration test")
	}
	stackOnce.Do(func() {
		stack, stackErr = bootStack()
		if stack != nil {
			// Stack outlives all tests in the package; cleanup runs
			// when the process exits.
			cleanup := stack.cleanup
			stack.cleanup = func() {} // prevent double-cleanup from within tests
			// Best-effort cleanup via runtime finalizer equivalent:
			// register an exit hook.
			testCleanups = append(testCleanups, cleanup)
		}
	})
	if stackErr != nil {
		t.Fatalf("booting test stack: %v", stackErr)
	}
	return stack
}

// testCleanups run at process exit via TestMain.
var testCleanups []func()

func TestMain(m *testing.M) {
	code := m.Run()
	for _, fn := range testCleanups {
		fn()
	}
	os.Exit(code)
}

func bootStack() (*testStack, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	pg, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("ghostcam_test"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		return nil, fmt.Errorf("starting postgres: %w", err)
	}

	pgURL, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = testcontainers.TerminateContainer(pg)
		return nil, fmt.Errorf("postgres conn string: %w", err)
	}

	rd, err := tcredis.Run(ctx, "redis:7-alpine")
	if err != nil {
		_ = testcontainers.TerminateContainer(pg)
		return nil, fmt.Errorf("starting redis: %w", err)
	}

	redisURL, err := rd.ConnectionString(ctx)
	if err != nil {
		_ = testcontainers.TerminateContainer(pg)
		_ = testcontainers.TerminateContainer(rd)
		return nil, fmt.Errorf("redis conn string: %w", err)
	}

	opts, err := goredis.ParseURL(redisURL)
	if err != nil {
		_ = testcontainers.TerminateContainer(pg)
		_ = testcontainers.TerminateContainer(rd)
		return nil, fmt.Errorf("redis parse url: %w", err)
	}
	redisClient := goredis.NewClient(opts)

	cleanup := func() {
		_ = redisClient.Close()
		_ = testcontainers.TerminateContainer(pg)
		_ = testcontainers.TerminateContainer(rd)
	}

	return &testStack{
		pgURL:       pgURL,
		redisURL:    redisURL,
		redisClient: redisClient,
		cleanup:     cleanup,
	}, nil
}

// newTestApp wires a fresh *App + httptest.Server per test. It uses a
// throwaway database created inside the shared Postgres container so
// tests never share state. Seeds the bootstrap admin so login flows
// have something to authenticate against.
func newTestApp(t *testing.T) (*App, *httptest.Server, string) {
	t.Helper()
	s := getStack(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Connect as superuser to create a per-test database, then reconnect
	// to that fresh database. Each test gets complete isolation without
	// paying the ~5s container-boot cost.
	dbName := fmt.Sprintf("t_%s", strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_")))
	dbName = sanitizeDBName(dbName)

	// Use a raw pgx connection (not the db package, which would run
	// migrations on the bootstrap database before we've even created
	// the per-test one).
	admin, err := pgx.Connect(ctx, s.pgURL)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %q`, dbName)); err != nil {
		admin.Close(ctx)
		t.Fatalf("create db %q: %v", dbName, err)
	}
	admin.Close(ctx)

	// Build per-test DSN by splicing the dbName into the shared URL.
	testURL := replaceDBName(s.pgURL, dbName)

	database, err := db.Connect(ctx, testURL)
	if err != nil {
		t.Fatalf("connect test db: %v", err)
	}

	initialPassword, err := database.Initialize(ctx, "integration-test-password", "admin@test.invalid")
	if err != nil {
		database.Close()
		t.Fatalf("initialize: %v", err)
	}
	if initialPassword != "integration-test-password" {
		t.Fatalf("expected preset password to be used, got %q", initialPassword)
	}

	hmacSecret, err := database.GetHMACSecret(ctx)
	if err != nil {
		database.Close()
		t.Fatalf("get hmac secret: %v", err)
	}

	cfg := &ServerConfig{
		DataDir:     t.TempDir(),
		HTTPPort:    0,
		DatabaseURL: testURL,
		AdminEmail:  "admin@test.invalid",
	}

	app := &App{
		Config:     cfg,
		DB:         database,
		Redis:      s.redisClient,
		S3:         nil, // auth seam doesn't touch S3
		HMACSecret: hmacSecret,
		Tiers:      billing.NewCache(),
		Mailer:     mailer.New(mailer.Config{}),
		Live:       NewLiveManager(),
		WHEP:       NewWHEPManager(),
		Triage:     triage.New(""),
		Linear:     linear.New(linear.Config{}),
	}

	srv := httptest.NewServer(app.router())
	t.Cleanup(func() {
		srv.Close()
		database.Close()
	})

	return app, srv, initialPassword
}

// sanitizeDBName strips anything not lowercase/digit/underscore and
// clips to the Postgres 63-byte identifier limit. Keeps `t_` prefix.
func sanitizeDBName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > 63 {
		out = out[:63]
	}
	return out
}

// replaceDBName rewrites the path portion of a postgres://… URL to
// point at a different database name. testcontainers gives us a URL
// with the default db inline; we swap in the per-test name.
func replaceDBName(url, newName string) string {
	// postgres://user:pass@host:port/dbname?opts
	q := ""
	if i := strings.Index(url, "?"); i >= 0 {
		q = url[i:]
		url = url[:i]
	}
	i := strings.LastIndex(url, "/")
	if i < 0 {
		return url + "/" + newName + q
	}
	return url[:i] + "/" + newName + q
}

// --- HTTP helpers ---

// doJSON POSTs a JSON body and returns status + raw body. Cookies are
// carried on the provided jar (pass nil to discard).
func doJSON(t *testing.T, client *http.Client, method, url string, body any) (int, []byte, *http.Response) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, buf, resp
}

// --- Seam 1: Auth ---
//
// Covers the same wire-up the old e2e auth.spec.ts did: POST /login
// must issue a ghostcam-token cookie that subsequent authenticated
// requests accept, and must reject wrong credentials.

func TestIntegration_Login_Success(t *testing.T) {
	_, srv, password := newTestApp(t)

	client := &http.Client{}
	status, body, resp := doJSON(t, client, http.MethodPost, srv.URL+"/api/v1/auth/login",
		apitypes.LoginRequest{Email: "admin@test.invalid", Password: password})

	if status != http.StatusOK {
		t.Fatalf("login status = %d, body=%s", status, body)
	}

	var got apitypes.LoginResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, body)
	}
	if got.UserID == "" {
		t.Error("UserID missing from login response")
	}

	// Cookie must be present and named ghostcam-token.
	setCookie := resp.Header.Get("Set-Cookie")
	if !strings.Contains(setCookie, "ghostcam-token=") {
		t.Errorf("Set-Cookie missing ghostcam-token: %q", setCookie)
	}
	if !strings.Contains(setCookie, "SameSite=Strict") {
		t.Errorf("cookie missing SameSite=Strict: %q", setCookie)
	}
}

func TestIntegration_Login_WrongPassword(t *testing.T) {
	_, srv, _ := newTestApp(t)

	status, _, resp := doJSON(t, &http.Client{}, http.MethodPost, srv.URL+"/api/v1/auth/login",
		apitypes.LoginRequest{Email: "admin@test.invalid", Password: "wrong-password"})

	if status != http.StatusUnauthorized {
		t.Errorf("wrong password: status = %d, want 401", status)
	}
	if c := resp.Header.Get("Set-Cookie"); strings.Contains(c, "ghostcam-token=") {
		t.Errorf("failed login leaked auth cookie: %q", c)
	}
}

func TestIntegration_Login_UnknownEmail(t *testing.T) {
	_, srv, _ := newTestApp(t)

	status, _, _ := doJSON(t, &http.Client{}, http.MethodPost, srv.URL+"/api/v1/auth/login",
		apitypes.LoginRequest{Email: "ghost@nowhere.invalid", Password: "anything"})

	if status != http.StatusUnauthorized {
		t.Errorf("unknown email: status = %d, want 401", status)
	}
}

// TestIntegration_JWT_Round_Trip is the real proof the auth stack is
// wired together: after login, the returned cookie must open an
// authenticated endpoint (viewerAuth middleware + JWT verify + pgx
// pool look-up all need to work).
func TestIntegration_JWT_Round_Trip(t *testing.T) {
	_, srv, password := newTestApp(t)

	// 1. Login, capture cookie.
	loginReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/auth/login",
		strings.NewReader(`{"email":"admin@test.invalid","password":"`+password+`"}`))
	loginReq.Header.Set("Content-Type", "application/json")
	loginResp, err := http.DefaultClient.Do(loginReq)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d", loginResp.StatusCode)
	}

	cookie := firstCookie(loginResp, "ghostcam-token")
	if cookie == "" {
		t.Fatal("login did not set ghostcam-token cookie")
	}

	// 2. Call an authenticated endpoint with that cookie.
	camerasReq, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/cameras", nil)
	camerasReq.AddCookie(&http.Cookie{Name: "ghostcam-token", Value: cookie})
	camerasResp, err := http.DefaultClient.Do(camerasReq)
	if err != nil {
		t.Fatalf("GET cameras: %v", err)
	}
	defer camerasResp.Body.Close()
	if camerasResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(camerasResp.Body)
		t.Fatalf("GET cameras status = %d, body=%s", camerasResp.StatusCode, b)
	}

	// 3. Same endpoint without the cookie must reject.
	noAuthReq, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/cameras", nil)
	noAuthResp, err := http.DefaultClient.Do(noAuthReq)
	if err != nil {
		t.Fatalf("GET cameras (no auth): %v", err)
	}
	noAuthResp.Body.Close()
	if noAuthResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET cameras without cookie: status = %d, want 401", noAuthResp.StatusCode)
	}
}

// TestIntegration_JWT_Tampered_Cookie_Rejected proves the server's JWT
// verification is wired correctly: a cookie whose payload has been
// edited (even if structurally valid) must be rejected by the
// middleware. This is the privilege-escalation invariant the auth
// package's unit tests cover — but here it runs through the live
// chi stack.
func TestIntegration_JWT_Tampered_Cookie_Rejected(t *testing.T) {
	app, srv, _ := newTestApp(t)

	// Forge a token with a user ID that doesn't exist, signed by a
	// *different* HMAC secret. The middleware must reject it.
	forged := auth.SignJWT("nonexistent-user", "attacker@example.com", true,
		[]byte("attacker-secret-not-ours"), time.Hour)

	// Sanity check: our own secret would NOT produce this token.
	if claims := auth.VerifyJWT(forged, app.HMACSecret); claims != nil {
		t.Fatal("test setup broken: forged token verified under real secret")
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/cameras", nil)
	req.AddCookie(&http.Cookie{Name: "ghostcam-token", Value: forged})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET cameras: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("forged cookie accepted: status = %d, want 401", resp.StatusCode)
	}
}

func firstCookie(resp *http.Response, name string) string {
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}
