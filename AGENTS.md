# Tentmaker: rules for agents working on this repo

Read `docs/DESIGN.md` first. It is the contract between packages; change it in the same PR as
any API change.

## Done check

A change is done when all of these pass:

```sh
go vet ./...
go test ./...
for os in linux darwin windows; do for arch in amd64 arm64; do GOOS=$os GOARCH=$arch go build ./... || exit 1; done; done
```

## Rules

- Go standard library first. A new dependency needs a reason in the PR and a pinned version.
- No cgo. Every binary is static and cross-compiled.
- No test touches the real host (root, podman, tart, the tailnet, the network). Use the fakes or
  a small interface; put real-host tests behind the `live` build tag.
- Secrets (tokens, JIT configs) never go in argv, an environment variable, a log line or an error
  message.
- Every external call has a timeout. Every state file is written with `config.WriteAtomic`.
- A probe that fails, fails closed: no new jobs.
- Plain American English in docs and messages. No em or en dashes.
- Commit messages: imperative, one line summary, body says why.
