# Turnstile token flow

The current image and conversation flow uses two different token layers.

## 1. Requirements seed

The Go provider builds a `p` value with `buildRequirementsToken`, using:

- the browser User-Agent of the page that will solve Turnstile
- the PoW script URL and data-build found on `chatgpt.com`
- the browser's device/session fingerprint

It posts that value to:

```text
POST /backend-api/sentinel/chat-requirements/prepare
{"p":"<p-token>"}
```

The response contains:

- `prepare_token`
- optional `proofofwork` seed/difficulty
- optional `turnstile.required` and `turnstile.dx`

## 2. Turnstile program

`dx` is a base64-encoded instruction program XOR-encrypted with `p`. Running it
requires the live `window`, `document`, `navigator`, `performance`, mouse state
and other browser data from a real `chatgpt.com` page.

The current page exposes a content-hashed Turnstile module:

```text
/unauth-mweb/assets/turnstile-<hash>.js
```

The solver imports that module and calls:

```js
turnstile.getEnforcementToken(requirements)
```

OpenAI's module keeps the seed in a `WeakMap`. The solver temporarily patches
`WeakMap.prototype.get` so the module receives the `p` value as its seed while
it already has the complete prepare response. The returned string is the
Turnstile token.

The built-in Go solver performs the import and call in one persistent
Chromium-family page. It has no local VM fallback. Since 2026-10-14 a token
produced by emulation alone is rejected by the conversation endpoint.

## 3. Finalize

The Go service posts the three values:

```text
POST /backend-api/sentinel/chat-requirements/finalize
{
  "prepare_token": "...",
  "proof_token": "...",
  "turnstile_token": "..."
}
```

The response supplies the final chat requirements token and optional
`so_token`.

## 4. Conversation headers

Subsequent conversation and image requests carry:

```text
OpenAI-Sentinel-Chat-Requirements-Token
OpenAI-Sentinel-Proof-Token
OpenAI-Sentinel-Turnstile-Token
OpenAI-Sentinel-SO-Token
```

If the Turnstile token was not produced against the same page environment and
UA, the upstream may return `403 Unusual activity` or disguise the failure as:

```json
{"detail":"Model not found"}
```

## Deployment minimum

The deployment layout is:

- one Go main process
- SQLite, image storage, and the embedded solver manager
- one headless Chromium page owned by the main process

The browser cost cannot be removed by rewriting the client in Go. Chromium is
multi-process. In the 2026-10-22 Windows Edge 155 test, the Go process tree used
about 270-285 MB of private memory after a successful solve, compared with about
400-420 MB for the Python/Playwright tree. Working set was higher and
fluctuated with shared pages. Budget roughly 500-800 MB for the browser and
prefer a 1 GB host.

The embedded solver was validated end-to-end with two real image jobs on
2026-10-22. One browser, one page, one image worker and a 30-minute page refresh
is the recommended low-load configuration.
