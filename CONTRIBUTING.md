# Contributing

Thanks for helping. Issues and pull requests are welcome. For a security problem, follow [SECURITY.md](SECURITY.md) instead of opening an issue.

## Before you write code

Read [docs/DESIGN.md](docs/DESIGN.md). It is the contract between packages, and [docs/CLI.md](docs/CLI.md) is the contract for the command line. A change to an API or a command changes the contract in the same pull request.

For anything bigger than a bug fix, open an issue first so we can agree on the shape.

## Done check

A change is done when all of these pass:

```sh
go vet ./...
go test ./...
for os in linux darwin windows; do for arch in amd64 arm64; do GOOS=$os GOARCH=$arch go build ./... || exit 1; done; done
```

CI also runs `gofmt -l .`, `go test -race` on Linux and macOS, shellcheck on `scripts/*.sh`, and both installers against a locally built release.

## Rules

- Go standard library first. A new dependency needs a reason in the PR and a pinned version.
- No cgo. Every binary is static and cross-compiled.
- No test touches the real host (root, podman, tart, the tailnet, the network). Use the fakes or a small interface. Real-host tests go behind the `live` build tag: `go test -tags live ./internal/power ./internal/guards ./internal/mesh`.
- Secrets (tokens, JIT configs) never go in argv, an environment variable, a log line or an error message.
- Every external call has a timeout. Every state file is written with `config.WriteAtomic`.
- A probe that fails, fails closed: no new jobs.
- Plain American English in docs and messages. No em or en dashes.
- Commit messages: imperative, a one-line summary, and a body that says why.

## How releases work

There are two channels and no others.

- **stable:** push a tag `vX.Y.Z`. The release workflow runs the tests, builds the six archives and `SHA256SUMS`, attests their provenance, and publishes a GitHub release. An annotated tag's message becomes the release notes. Tags like `v1.0.0-rc1` are rejected.
- **dev:** every push to `main` rebuilds the rolling prerelease tagged `dev`. Its version is the patch after the newest stable tag plus `-dev.<UTC yyyymmddHHMM>.<sha7>`, like `0.1.1-dev.202611211530.abc1234`, and the release title is that exact version.

Devices on a channel pick up a new release on their next update check, between jobs.

## License

By contributing you agree that your work is released under the [MIT License](LICENSE).
