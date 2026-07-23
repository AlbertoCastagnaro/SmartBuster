# make ui  — build the Svelte web UI and embed it into the daemon's
#            asset mount (spec smartbuster-phase5b-spec.md §2): the only
#            step required before a `go build` produces a single binary
#            that serves the real dashboard instead of the phase 5a
#            placeholder. Re-run after any web/ change.
# make build — ui, then the smartbuster binary itself.
#
# For iteration, `cd web && npm run dev` (proxying to a running
# `smartbuster serve`) is faster than rebuilding the embedded bundle on
# every change — see web/vite.config.ts's VITE_DAEMON_TARGET.

.PHONY: ui build test clean-ui-assets

ui: clean-ui-assets
	cd web && npm install && npm run build
	cp -r web/dist/. internal/daemon/assets/

# Removes prior `make ui` output (Vite's output filenames are
# content-hashed, so stale chunks would otherwise accumulate) while always
# preserving PLACEHOLDER.md — see .gitignore's note on why that file must
# never be removed from the directory.
clean-ui-assets:
	find internal/daemon/assets -mindepth 1 ! -name PLACEHOLDER.md -delete

build: ui
	go build -o smartbuster ./cmd/smartbuster

test:
	go test ./...
	cd web && npm test
