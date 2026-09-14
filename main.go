// camada-go-example: a small net/http app wired with camada against a local edge-analyst.
// Setup: cp .env.example .env (paste the CAMADA_KEY printed by `npm run seed`), go run .
// (PORT=3003 by default). RemoteAddr is the socket peer, so nothing needs disabling for
// proxies: a reverse proxy in front of this app is declared through CAMADA_TRUSTED_PROXY or
// the tenant config, never inferred from a header.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"syscall"
	"time"

	"github.com/camada-app/camada-go"
)

var envLine = regexp.MustCompile(`^([A-Z_]+)=(.*)$`)

// loadDotEnv is a minimal .env loader so the example has zero extra dependencies: it fills
// variables the environment does not already set.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return // no .env: rely on the environment
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if m := envLine.FindStringSubmatch(sc.Text()); m != nil && os.Getenv(m[1]) == "" {
			_ = os.Setenv(m[1], m[2])
		}
	}
}

func page(w http.ResponseWriter, r *http.Request, status int, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html>
<html><head><meta charset="utf-8"><title>%s</title>%s</head>
<body style="font-family: system-ui; max-width: 40rem; margin: 3rem auto">
<nav><a href="/">home</a> · <a href="/pricing">pricing</a> · <a href="/login-form">login</a> · <a href="/challenge-me">challenge</a></nav>
<h1>%s</h1>%s</body></html>`, title, camada.ScriptTag(r), title, body)
}

// newApp is the routed app behind the camada middleware; the engine builds itself on the
// first request (the two-line install).
func newApp() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		page(w, r, 200, "camada example shop", `
  <p>Every request here is captured by camada; the beacon below fingerprints this browser first-party.</p>
  <p><button onclick="fetch('/api/data').then(r=>r.json()).then(d=>alert(JSON.stringify(d)))">call the API</button></p>`)
	})
	mux.HandleFunc("GET /pricing", func(w http.ResponseWriter, r *http.Request) {
		page(w, r, 200, "Pricing", "<p>Free while unreleased.</p>")
	})
	mux.HandleFunc("GET /login-form", func(w http.ResponseWriter, r *http.Request) {
		page(w, r, 200, "Log in", `
  <form method="post" action="/login">
    <input name="user" placeholder="email"> <input name="pass" type="password"> <button>go</button>
  </form>`)
	})
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm() // a body that is not a form is a failed login, not a 500
		user, pass := r.PostForm.Get("user"), r.PostForm.Get("pass")
		outcome, status, title, result := "login_failed", 401, "Nope", "failed"
		if user == "demo@example.com" && pass == "demo" {
			outcome, status, title, result = "login_succeeded", 200, "Welcome", "succeeded"
		}
		camada.Track(r, outcome, user) // uid is HMAC-hashed in the SDK
		page(w, r, status, title, fmt.Sprintf("<p>login %s</p>", result))
	})
	mux.HandleFunc("GET /api/data", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "at": time.Now().UnixMilli()})
	})
	// SDK-04 demo: force the challenge for this route, whatever the snapshot says. In production
	// the same page is served automatically for a `challenge` verdict. Once solved, the `_cch`
	// cookie is good for an hour and this route renders normally.
	mux.HandleFunc("GET /challenge-me", func(w http.ResponseWriter, r *http.Request) {
		if camada.ServeChallenge(w, r) {
			return
		}
		page(w, r, 200, "Challenge passed", `
    <p>The <code>_cch</code> cookie is set for an hour. Clear it (or open a private window) to see the check again.</p>`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		page(w, r, 404, "404", "<p>Nothing here.</p>")
	})
	return camada.Handler(mux) // ← the two-line install; the engine builds itself on the first request
}

// serve runs srv on ln until ctx is done, then stops accepting, waits for in-flight requests
// and drains camada's last event batch before returning. ListenAndServe returns the moment
// Shutdown closes the listener, so main must wait here rather than exit on that return: the
// drain would otherwise be cut off mid-flight and the last ~15 s of events dropped.
func serve(ctx context.Context, srv *http.Server, ln net.Listener) error {
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
		camada.Default().Stop(shutdown) // drains the last event batch, at most 500 ms
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err // the listener failed on its own: nothing is being shut down
	}
	<-done
	return nil
}

func main() {
	loadDotEnv(".env")
	port := os.Getenv("PORT")
	if port == "" {
		port = "3003"
	}
	srv := &http.Server{Handler: newApp(), ReadHeaderTimeout: 10 * time.Second}
	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("camada-go-example listening on :%s", port)
	if err := serve(ctx, srv, ln); err != nil {
		log.Fatal(err)
	}
}
