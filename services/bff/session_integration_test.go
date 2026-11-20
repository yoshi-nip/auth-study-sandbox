package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/oauth2"
)

// 実 Redis を使う。通常の単体テストではスキップし、E2E の隔離コンテナで必ず実行する。
// ホストへの Redis ポート公開や、実際のユーザーセッションの読み出しは不要。
func redisStore(t *testing.T) *sessionStore {
	t.Helper()
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("E2E container supplies TEST_REDIS_ADDR")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	return &sessionStore{rdb: rdb, idle: time.Minute, absolute: time.Hour}
}

func testSession(t *testing.T, store *sessionStore) (string, *session) {
	t.Helper()
	s := &session{Subject: "test-user", RefreshToken: "test-old-refresh", AccessToken: "test-old-access", AccessExpiry: time.Now().Add(-time.Minute)}
	sid, err := store.create(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.delete(context.Background(), sid) })
	return sid, s
}

func TestRedisUpdateDoesNotResurrect(t *testing.T) {
	store := redisStore(t)
	ctx := context.Background()
	sid, s := testSession(t, store)
	s.AccessToken = "test-new-access"
	if err := store.update(ctx, sid, s); err != nil {
		t.Fatal(err)
	}
	latest, err := store.take(ctx, sid)
	if err != nil || latest == nil || latest.AccessToken != s.AccessToken {
		t.Fatal("take must return newest stored session")
	}
	if err := store.update(ctx, sid, s); !errors.Is(err, errSessionGone) {
		t.Fatalf("update after logout: %v", err)
	}
	// 期限切れも実 Redis の期限で再現し、保存できないことを確かめる。
	sid2, s2 := testSession(t, store)
	if err := store.rdb.PExpire(ctx, sessionKey(sid2), time.Millisecond).Err(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := store.update(ctx, sid2, s2); !errors.Is(err, errSessionGone) {
		t.Fatalf("update after expiry: %v", err)
	}
}

func TestRedisLogoutDuringRefresh(t *testing.T) {
	store := redisStore(t)
	sid, s := testSession(t, store)
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	revoked := make(chan string, 4)
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			close(started)
			<-release // トークン発行を止め、必ずその間にログアウトする（sleep 頼みの競合にしない）
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"test-new-access","refresh_token":"test-new-refresh","token_type":"Bearer","expires_in":300}`))
			return
		}
		revoked <- r.PostFormValue("token")
		w.WriteHeader(http.StatusOK)
	}))
	defer idp.Close()
	defer unblock()
	a := &app{
		sessions:   store,
		cfg:        config{appOrigin: "https://app.localhost:8443", refreshSkew: time.Second, revocationURL: idp.URL + "/revoke"},
		oauth:      &oauth2.Config{ClientID: "test", ClientSecret: "test", Endpoint: oauth2.Endpoint{TokenURL: idp.URL + "/token", AuthStyle: oauth2.AuthStyleInHeader}},
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := a.freshToken(ctx, sid, s); done <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("refresh did not start")
	}
	r := httptest.NewRequest("POST", "/bff/logout", strings.NewReader(""))
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	r.Header.Set("Origin", a.cfg.appOrigin)
	r.Header.Set("X-CSRF-Token", s.CSRFToken)
	w := httptest.NewRecorder()
	a.withSession(a.withCSRF(http.HandlerFunc(a.logout))).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("logout status %d", w.Code)
	}
	unblock()
	select {
	case err := <-done:
		if !errors.Is(err, errSessionGone) {
			t.Fatalf("refresh after logout: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("refresh did not finish")
	}
	if exists, err := store.rdb.Exists(ctx, sessionKey(sid)).Result(); err != nil || exists != 0 {
		t.Fatal("session resurrected or Redis failed")
	}
	// 古い Cookie を再送しても拒否する。
	w = httptest.NewRecorder()
	a.withSession(http.HandlerFunc(a.sessionInfo)).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("old cookie accepted: %d", w.Code)
	}
	seen := map[string]bool{}
	for len(revoked) > 0 {
		seen[<-revoked] = true
	}
	if !seen["test-old-refresh"] || !seen["test-new-refresh"] {
		t.Fatal("both old and late-issued refresh tokens must be revoked")
	}
}

// Redis コマンド境界で障害を注入する。GET の失敗と GETDEL の失敗を別々に確かめる。
type failCommandHook struct{ command string }

func (h failCommandHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h failCommandHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h failCommandHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == h.command {
			return errors.New("injected Redis failure")
		}
		return next(ctx, cmd)
	}
}

func TestRedisLogoutFailureIsNotSuccess(t *testing.T) {
	for _, command := range []string{"get", "getdel"} {
		t.Run(command, func(t *testing.T) {
			store := redisStore(t)
			sid, s := testSession(t, store)
			store.rdb.AddHook(failCommandHook{command: command})
			a := &app{sessions: store, cfg: config{appOrigin: "https://app.localhost:8443"}}
			r := httptest.NewRequest("POST", "/bff/logout", nil)
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
			r.Header.Set("Origin", a.cfg.appOrigin)
			r.Header.Set("X-CSRF-Token", s.CSRFToken)
			w := httptest.NewRecorder()
			a.withSession(a.withCSRF(http.HandlerFunc(a.logout))).ServeHTTP(w, r)
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("Redis failure returned %d", w.Code)
			}
			if strings.Contains(w.Body.String(), "logout_url") {
				t.Fatal("false logout success")
			}
			if len(w.Result().Cookies()) != 0 {
				t.Fatal("cleared cookie despite failed logout")
			}
			if n, err := store.rdb.Exists(context.Background(), sessionKey(sid)).Result(); err != nil || n != 1 {
				t.Fatal("session unexpectedly removed")
			}
		})
	}
}
