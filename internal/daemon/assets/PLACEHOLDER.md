# Phase 5b asset mount

This directory is `internal/daemon`'s embedded static-asset root (see
`assets.go`), served at `/` by `smartbuster serve` for everything not under
`/api/`. Phase 5a intentionally leaves it empty — Phase 5b fills it with the
built web UI.
