# camada-go-example

A `net/http` app wired with [`camada-go`](../camada-go) against a locally running edge-analyst.
This is the hand-test bench for the Go SDK, the twin of [`camada-python-example`](../camada-python-example).

## Setup

1. Terminal A — `cd ../camada/edge-analyst && npm run dev` (analyst on :8787), then
   `npm run seed` in a second terminal. Note the printed `CAMADA_KEY`.
2. Here: `cp .env.example .env` (paste the key if it differs), then `go run .` →
   http://localhost:3003 (`PORT` overrides). `go.mod` replaces `github.com/camada-app/camada-go`
   with the sibling checkout `../camada-go`, the way the npm examples use `file:` deps.

Nothing needs disabling for proxies: `net/http` hands the SDK the socket peer (`r.RemoteAddr`)
and never rewrites it from a header, so `X-Forwarded-For` counts only under the SDK's
trusted-proxy rules (`CAMADA_TRUSTED_PROXY`, or the tenant config). A reverse proxy in front of
this app is declared, not inferred.

## Hand test (what to look for)

1. Browse `http://localhost:3003` — the page renders; devtools → Network shows
   `/_cam/b.js?r=<uuid>` (200, JavaScript) and ~2 s later `POST /_cam/fp` (204). The document
   response carries an `X-Rid` header and sets the `_sfp` cookie.
2. Click/type before the beacon fires — the `/_cam/fp` payload's `input` counters are non-zero.
3. `curl -i http://localhost:3003/ -H 'X-Forwarded-For: 203.0.113.66'` → **403** with
   `x-block-reason: rule` and `x-block-rule: builtin:block` (the seed blocks that IP as an entry
   of the built-in block list on snapshot v5). The very first request after boot answers **200**
   instead — see below.
4. Fail three logins (`demo@example.com` / anything wrong) via `/login-form`, then run an
   analysis (`curl -s -X POST -H 'authorization: Bearer dev' 'http://localhost:8787/admin/run?tenant=acme&minutes=10'`) —
   events include `login_failed` rows with a hashed `uid`; your raw email appears nowhere.
5. Kill the analyst (Ctrl-C in terminal A) and reload the page + `curl localhost:3003/api/data` —
   everything still answers 200; the app's terminal shows at most one
   `[camada] suppressed error` line a minute. That is fail-open.
6. Restart this app with `CAMADA_DISABLED=1 go run .` — no `X-Rid` header, no `/_cam/b.js`
   requests: the kill switch bypasses the SDK entirely.

## The first request after boot is cold

`camada.Handler(mux)` builds one engine lazily, on the first request through the middleware.
That build starts the snapshot poll on a goroutine and never blocks, so the request that
triggered it is matched against an empty snapshot and falls open: a `curl` from the blocked IP
right after boot gets `200` with an `X-Rid`, and requests keep passing until that first poll
lands — a few hundred milliseconds against a local analyst (the e2e measures ~260 ms with a
250 ms probe; the figure is snapshot-size and network bound), then `403`.
`node scripts/e2e-sdk-go.mjs` in `../camada/edge-analyst` makes that first request itself
and asserts both answers.

If request 1 must be enforced, build the engine in `main` and wait for the boot poll:

```go
import "github.com/camada-app/camada-go/snapshot"

cam := camada.Default()   // builds the engine; the boot poll is already running on its goroutine
if cam.Snap != nil {      // nil when CAMADA_KEY is unset or CAMADA_DISABLED=1
	deadline := time.Now().Add(5 * time.Second)
	for cam.Snap.Verdict(snapshot.MatchInput{IP: "0.0.0.0"}).Reason == "cold" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)   // bounded: an unreachable analyst leaves it cold, and the app still fails open
	}
}
```

`Snap.Refresh()` is not the warm-up: the boot poll holds the single-in-flight lock, so a
synchronous `Refresh()` called right after `Default()` returns at once and the engine is still
cold.

## Challenge (SDK-04)

`/challenge-me` forces the first-party proof-of-work challenge, whatever the snapshot says — the
same page camada serves automatically for a `challenge` verdict.

1. Browse `http://localhost:3003/challenge-me` — "Checking your browser" appears, the inline
   solver hunts a SHA-256 with 16 leading zero bits (tens of milliseconds), the hidden form
   posts to `/__camada/challenge`, and the browser lands on "Challenge passed". Devtools shows
   the `_cch` cookie (`HttpOnly`, `SameSite=Lax`, one hour); reload and the page renders at once.
2. `curl -i http://localhost:3003/challenge-me -H 'accept: text/html' -H 'sec-fetch-dest: document'`
   → **403** with `x-camada-challenge: 1` and the page in the body.
3. Without an HTML `Accept` (an API client, an image, a fetch) the answer is
   `403 {"error":"challenge_required"}` instead — a status a client can act on rather than a
   page it cannot solve.
4. The events tell the two apart: a served challenge ships `st: 403, blk: "challenge"`, a passed
   one ships `st: 200, ch: 1`.

The nonce and the cookie are bound to the client IP, so camada serves no challenge to a request
it cannot identify (no trusted-proxy config and no socket address). Clear `_cch` — or open a
private window — to see the check again.

## Tests

`gofmt -l . | (! grep .) && go vet ./... && go test ./...` — the routes against an engine whose
analyst URL is a closed port (cold, fail open; the challenge needs no snapshot), a guard
that the `replace` directive resolves to the sibling SDK's current `version.go`, and the
shutdown path: `serve` returns only after camada drained its queue, so an outcome tracked just
before SIGTERM still reaches ingest.
