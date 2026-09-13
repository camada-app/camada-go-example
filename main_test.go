package main

// The example against a configured engine that can never load a snapshot: the analyst URL is a
// closed port, so every request runs cold and falls open, and the routes the app gates itself
// (challenge) still work because the challenge kit needs no snapshot.

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/camada/camada-go"
)

const blockedIP = "203.0.113.66"

var browser = map[string]string{"x-forwarded-for": "198.51.100.7", "accept": "text/html", "sec-fetch-dest": "document"}

var ridRE = regexp.MustCompile(`^[0-9a-f-]{36}$`)

func closedPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// server is the app over an engine built for this test alone (the lazy first-request build in
// production; here Configure so the previous test's stopped engine is never reused).
func server(t *testing.T) *httptest.Server {
	t.Helper()
	dead := "http://" + closedPort(t)
	engine := camada.Configure(camada.Options{Env: map[string]string{
		"CAMADA_KEY": "tok-example.snap-example", "CAMADA_INGEST_URL": dead, "CAMADA_SNAPSHOT_URL": dead + "/snapshot", "CAMADA_TRUSTED_PROXY": "hops:1",
	}})
	srv := httptest.NewServer(newApp())
	t.Cleanup(func() {
		srv.Close()
		engine.Stop(context.Background())
	})
	return srv
}

type reply struct {
	status  int
	headers http.Header
	body    string
}

func do(t *testing.T, srv *httptest.Server, method, path string, headers map[string]string, body string) reply {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != "" && req.Header.Get("content-type") == "" {
		req.Header.Set("content-type", "application/x-www-form-urlencoded")
	}
	res, err := http.DefaultTransport.RoundTrip(req) // no redirects, no cookie jar
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return reply{res.StatusCode, res.Header, string(b)}
}

func TestPagesRenderWithTheFirstPartyBeacon(t *testing.T) {
	srv := server(t)
	first := do(t, srv, "GET", "/", nil, "")
	if first.status != 200 || !strings.HasPrefix(first.headers.Get("set-cookie"), "_sfp=") {
		t.Fatalf("%+v", first)
	}
	for _, path := range []string{"/", "/pricing", "/login-form"} {
		r := do(t, srv, "GET", path, nil, "")
		rid := r.headers.Get("x-rid")
		if r.status != 200 || !ridRE.MatchString(rid) || !strings.Contains(r.body, "/_cam/b.js?r="+rid) {
			t.Fatalf("%s: %+v", path, r)
		}
	}
	if do(t, srv, "GET", "/_cam/b.js", nil, "").headers.Get("content-type") != "application/javascript" {
		t.Fatal("beacon script")
	}
}

func TestAPIAnswersJSON(t *testing.T) {
	r := do(t, server(t), "GET", "/api/data", nil, "")
	if r.status != 200 || !strings.Contains(r.body, `"ok":true`) {
		t.Fatalf("%+v", r)
	}
}

func TestLoginReportsTheOutcome(t *testing.T) {
	srv := server(t)
	bad := do(t, srv, "POST", "/login", nil, "user=demo%40example.com&pass=nope")
	if bad.status != 401 || !strings.Contains(bad.body, "login failed") {
		t.Fatalf("%+v", bad)
	}
	ok := do(t, srv, "POST", "/login", nil, "user=demo%40example.com&pass=demo")
	if ok.status != 200 || !strings.Contains(ok.body, "login succeeded") {
		t.Fatalf("%+v", ok)
	}
	// a body that is not UTF-8 is a failed login, not a 500
	if raw := do(t, srv, "POST", "/login", nil, "user=\xff&pass=x"); raw.status != 401 {
		t.Fatalf("%+v", raw)
	}
}

func TestEachTestBuildsItsOwnEngine(t *testing.T) {
	srv := server(t)
	do(t, srv, "GET", "/", nil, "")
	engine := camada.Default()
	if engine.Env == nil || !strings.HasSuffix(engine.Env.SnapshotURL, "/snapshot") || !strings.HasPrefix(engine.Env.SnapshotURL, "http://127.0.0.1:") {
		t.Fatalf("%+v", engine.Env)
	}
}

func TestAColdSnapshotFallsOpen(t *testing.T) {
	r := do(t, server(t), "GET", "/", map[string]string{"x-forwarded-for": blockedIP}, "")
	if r.status != 200 || r.headers.Get("x-block-reason") != "" || r.headers.Get("x-rid") == "" {
		t.Fatalf("%+v", r)
	}
}

func TestChallengeMeServesThePageToABrowser(t *testing.T) {
	r := do(t, server(t), "GET", "/challenge-me", browser, "")
	if r.status != 403 || r.headers.Get("x-camada-challenge") != "1" || !strings.HasPrefix(r.headers.Get("content-type"), "text/html") || !strings.Contains(r.body, "/__camada/challenge") {
		t.Fatalf("%+v", r)
	}
}

func TestChallengeMeAnswersJSONToAnAPIClient(t *testing.T) {
	r := do(t, server(t), "GET", "/challenge-me", map[string]string{"x-forwarded-for": "198.51.100.7"}, "")
	if r.status != 403 || r.body != `{"error":"challenge_required"}` {
		t.Fatalf("%+v", r)
	}
}

func TestUnknownPathRendersThe404Page(t *testing.T) {
	r := do(t, server(t), "GET", "/nope", nil, "")
	if r.status != 404 || !strings.Contains(r.body, "Nothing here") || r.headers.Get("x-rid") == "" {
		t.Fatalf("%+v", r)
	}
}

func TestTheReplaceDirectiveResolvesToTheSiblingSDK(t *testing.T) {
	// go.mod replaces camada-go with ../camada-go: the version this binary carries must be the sibling's literal
	versionGo := filepath.Join("..", "camada-go", "version.go")
	src, err := os.ReadFile(versionGo)
	if err != nil {
		t.Fatalf("no camada-go checkout beside this repo (%s)", versionGo)
	}
	m := regexp.MustCompile(`(?m)^const Version = "([^"]+)"`).FindSubmatch(src)
	if m == nil || string(m[1]) != camada.Version {
		t.Fatalf("camada.Version %q is not the sibling's %q", camada.Version, m)
	}
}

// The shutdown path: once ctx is done, serve closes the listener, waits for in-flight requests,
// and only returns after camada drained its queue — an outcome tracked just before SIGTERM
// still reaches ingest. (A fake ingest here; the snapshot URL stays a closed port.)
func TestServeDrainsTheLastEventBatchBeforeReturning(t *testing.T) {
	var mu sync.Mutex
	var rows []map[string]any
	ingest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/e" {
			var batch []map[string]any
			_ = json.NewDecoder(r.Body).Decode(&batch)
			mu.Lock()
			rows = append(rows, batch...)
			mu.Unlock()
			w.WriteHeader(202)
			return
		}
		w.WriteHeader(404)
	}))
	defer ingest.Close()
	engine := camada.Configure(camada.Options{Env: map[string]string{
		"CAMADA_KEY": "tok-example.snap-example", "CAMADA_INGEST_URL": ingest.URL, "CAMADA_SNAPSHOT_URL": "http://" + closedPort(t) + "/snapshot",
	}})
	t.Cleanup(func() { engine.Stop(context.Background()) })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serve(ctx, &http.Server{Handler: newApp()}, ln) }()

	srv := &httptest.Server{URL: "http://" + ln.Addr().String()}
	if r := do(t, srv, "POST", "/login", nil, "user=demo%40example.com&pass=nope"); r.status != 401 {
		t.Fatalf("%+v", r)
	}
	mu.Lock()
	early := len(rows)
	mu.Unlock()
	if early != 0 {
		t.Fatalf("the queue flushed before shutdown (%d rows): the test proves nothing", early)
	}
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after ctx was cancelled")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, row := range rows {
		if row["et"] == "login_failed" {
			return
		}
	}
	t.Fatalf("login_failed never reached ingest; rows %v", rows)
}
