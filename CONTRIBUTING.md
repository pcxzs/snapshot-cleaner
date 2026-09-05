# Contributing

Thanks for looking. This tool deletes files from read-only snapshots, so the bar
for changes to the destructive path is deliberately high.

## Before you start

Open an issue first for anything larger than a bug fix. `doctor` output from an
affected machine (`sudo snapshot-cleaner doctor`) turns most layout and
detection reports into something fixable in one pass.

## Development

```sh
make build      # ./snapshot-cleaner
make debug      # ./snapshot-cleaner-debug, logs everything to a file
make test       # unit tests, no root needed
make vet
make fmt
```

CI runs `gofmt -l`, `go vet ./...`, `go test -race ./...` and a cross-build for
`linux/amd64` and `linux/arm64`. Run `make fmt vet test` before pushing and it
will pass.

### Integration tests

The unit tests cover the parsers and the accounting. The integration tests cover
everything that touches a real filesystem, and they need root:

```sh
sudo SNAPSHOT_CLEANER_INTEGRATION=1 make integration
```

They build a throwaway btrfs filesystem in a loopback image and do the
destructive work there — they never touch the host's own snapshots. `make
integration` compiles the test binary as your normal user and runs only that
binary under `sudo`, so the Go build cache stays unpolluted.

CI cannot run them (no root, no btrfs), so it only proves they still compile.
**Run them locally for any change to scanning, measurement, the cache, or
purge**, and say so in the pull request.

## What a change needs

- `gofmt`-clean, `go vet`-clean.
- A test. New parsing or accounting behaviour gets a unit test; anything on the
  purge or cache path gets an integration test.
- No new runtime dependencies. The binary is static, CGO-free, and shells out to
  nothing — no `btrfs` CLI, no `compsize`. Kernel interfaces are reached through
  ioctls directly, and that stays true.
- Comments that explain *why*, in the style already in the tree. The existing
  code documents the reasoning behind non-obvious choices; match that.
- Honest output. When a figure is an estimate it is marked `~`; when it is
  unknown it prints `?`. Do not paper over a failure with a plausible number.

## Purge-path changes

The safety properties asserted by the integration tests are the contract:

- A partial purge frees nothing, so it must not report that it did.
- Every snapshot is intact and read-only when the run ends, including after a
  failure or an interrupt.
- Received (send/receive) snapshots are refused, not silently modified.
- A purge invalidates the cached listings of the snapshots it touched.

If a change makes one of those harder to guarantee, say so in the pull request
rather than adjusting the test.

## Commits and pull requests

One logical change per commit, with a subject line that says what changed and a
body that says why. Describe in the pull request what you ran — unit tests,
integration tests, and on what layout (Timeshift, snapper, btrbk or a manual
`pair`).

## License

Contributions are accepted under [GPL-3.0-or-later](LICENSE), the license of the
project.
