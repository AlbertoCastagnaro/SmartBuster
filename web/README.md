# smartbuster web UI

Svelte 5 + TypeScript + Vite dashboard for `smartbuster serve` (Phase 5b —
see `../specs/smartbuster-phase5b-spec.md`). Renders the daemon's REST +
WS protocol frozen by Phase 5a (`../specs/smartbuster-phase5a-spec.md`);
adds no engine behavior of its own.

## Production: the embedded single binary

```
make ui     # npm install && vite build -> web/dist/ -> internal/daemon/assets/
make build  # ui, then go build -o smartbuster ./cmd/smartbuster
```

The resulting binary serves the UI directly — no external assets, no
runtime npm. Run from the repo root, not from `web/`.

## Development

```
smartbuster serve --port 8899   # a fixed port so the dev proxy target is stable
cd web && npm run dev           # vite dev on :5173, proxying /api to :8899
```

Point at a different daemon port/host with `VITE_DAEMON_TARGET` (see
`vite.config.ts`) if not using 8899. Open the URL `smartbuster serve`
printed (with its `#token=...`), but on port 5173 instead of the daemon's
own port.

## Tests

```
npm run check   # svelte-check + tsc
npm test        # vitest — store.ts's reducer against a recorded event
                # fixture (src/fixtures/scan-events.raw.jsonl), no browser
```
