# Development

## Requirements

- Go 1.27.1 or later (the `go` directive in `go.mod`).
- No C compiler: everything, including the SQLite driver used in tests
  (modernc.org/sqlite), is pure Go. The race detector needs cgo.

## Build and test

```bash
go build ./...
go vet -copylocks=false ./...
go test ./...
go test -race -count=1 ./...

# the connector is its own module
cd connectors/plc4x && go test ./...
```

`-copylocks=false`: `GetTag` and friends return `Tag` by value, which vet's
copylocks check reports; every other vet check runs.

## Layout

| Path | Contents |
|---|---|
| `*.go` (root) | The `honeycomb` package; see [Architecture](architecture.md) |
| `store/sqlstore/` | SQL `TagStore`, dialects, migrations under `migrations/<dialect>/` |
| `connectors/plc4x/` | The PLC4X connector module |
| `shared/` | Sample types for the examples |
| `examples/` | Runnable examples (`go run ./examples/simple`) |
| `doc/` | These pages |

## CI

`.github/workflows/`:

| Workflow | Jobs |
|---|---|
| `go.yml` | Build, vet and test on Linux, macOS and Windows with the minimum Go and stable; the PLC4X connector on the three systems; gofmt and the race detector (the connector one package at a time for memory); Raspberry Pi (ARMv6, ARMv7, arm64) under QEMU |
| `security.yml` | govulncheck for both modules, gosec to the Security tab, `go mod verify` and `go mod tidy -diff`; weekly as well |
| `release.yml` | On a `v*` tag: race tests, then a GitHub release; a tag with a suffix (`v0.2.0-beta1`) is a pre-release |

## Releases

- Core releases are tagged `vX.Y.Z` (with `-betaN` for pre-releases).
- The connector is tagged `connectors/plc4x/vX.Y.Z`, as Go requires for a
  nested module.
- Tag only a commit whose CI is green; `release.yml` runs the race tests
  again before publishing.

## Conventions

- Every exported identifier has a doc comment; comments explain why, not
  what.
- A behaviour change comes with a test; concurrency changes with a test
  that fails under `-race` without the change.
- Errors name the tag and the operation (`SetTagValue: tag 'X' not found`).
- Keep the core free of third-party dependencies beyond royaljelly.

## Known issues

- **UDT field reads and the race detector.** Until the fix that reads a
  UDT field under the owning tag's lock (`fieldOf`, tested by
  `udt_field_race_test.go`), `GetTagValue("Tag.Field")` and
  `("Arr[i].Field")` read the field after releasing the lock, racing with
  field writes. Releases up to v0.2.0-beta1 have this race; beeguard's
  tagbridge race test found it.
- **Shared values in copies.** `GetTag`, `GetAllTags` and subscription
  updates copy the tag but not a UDT pointer or a slice; reading fields of
  such a copy while the database writes them is a race. Read through the
  database (`GetTagValue("Tag.Field")`) instead.
- **`Alias` is not a lookup name.** The README says tags can be accessed by
  alias; reads and writes do not resolve `Alias`.
- **README field names.** The README's `Tag` section lists `Forced` and
  `ForceValue`; the struct has `Force *ForceInfo`.
- **Copying locks.** `Tag` holds its mutex, so returning it by value needs
  `vet -copylocks=false`. A future API could return a lock-free view type.
