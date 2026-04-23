# Contributing to vertex-go

Thanks for your interest!

This repo is the **Go implementation** of Vertex. The spec lives at [dengxuan/Vertex](https://github.com/dengxuan/Vertex); please read carefully before proposing changes that touch the wire format.

## Types of changes

### A. Pure Go change (bug fix, refactor, perf, new test)

Single PR here. Standard review. No cross-repo coordination needed.

### B. Change that affects the wire format

You must also open a companion PR in the [Vertex spec repo](https://github.com/dengxuan/Vertex). Process:

1. Open an issue in the Vertex spec repo first, tag `spec`.
2. In the spec repo, update `/spec/wire-format.md` (or related) in one PR.
3. In this repo, implement the conformant change in a companion PR. Link the spec PR.
4. Both PRs must be reviewed; the spec PR merges last (after impl PRs on all impacted language repos are approved and green).
5. `/compat/` tests in the spec repo run the multi-language matrix.

### C. Shared `.proto` change

Follow the process in the Vertex spec repo's CONTRIBUTING.md § B. This repo regenerates its proto code after the shared protos update.

## Branch / PR conventions

- [Conventional Commits](https://www.conventionalcommits.org/): `feat:`, `fix:`, `docs:`, `chore:`, `refactor:`, `test:`, `perf:`, `ci:`.
- PR title = commit title (we squash-merge single-commit PRs).
- PR description: what changed, why, test plan.

## Versioning

Go modules use git tags:

- tag `vX.Y.Z` → users can `go get github.com/dengxuan/vertex-go@vX.Y.Z`
- tag `vX.Y.Z-rc.N` / `-preview.N` → pre-release consumption
- Go's semver handling will select the latest `vX` by default

The **wire spec** is versioned independently from this module. Multiple module versions can implement the same wire version.

### Go module v2+

If/when a breaking release happens, follow the Go module `v2+` convention: the import path changes to `github.com/dengxuan/vertex-go/v2/...`. Do not do this casually — only for wire v2 or a deliberate Go-only API break.

## Local development

```bash
go mod download
go vet ./...
go test -race -cover ./...
```

### Regenerating protos from the spec repo

```bash
# (script lands with the first implementation commit)
./scripts/sync-protos.sh
```

## Testing against other implementations

```bash
# from the Vertex spec repo
cd ../Vertex/compat
make fetch-impls
make run-all
```

## Where else to go

- **Wire-format questions** — [Vertex spec repo issues](https://github.com/dengxuan/Vertex/issues)
- **.NET implementation** — [vertex-dotnet](https://github.com/dengxuan/vertex-dotnet)
