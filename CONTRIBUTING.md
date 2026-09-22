# Contributing

Thank you. Small, focused pull requests against `main` are the easiest to
review. Open an issue first for anything that changes policy defaults, exit
codes, finding codes or the JSON report; those are public interfaces.

## Setup

```sh
brew install ffmpeg mkvtoolnix     # or your distro's packages
make build && ./bin/amuxify doctor
make test                          # vet + unit tests, no media tools needed
make difftest                      # differential test against legacy/, needs the tools
```

Go 1.26 or newer, `gofmt` clean (`make fmt`). No new dependencies without an
issue explaining why the standard library is not enough.

## Rules of the house

- Platform-independent code only. No vendor-specific CLIs or services in the
  build, the tests, or the release pipeline beyond generic GitHub Actions and
  goreleaser.
- Never weaken a safety guarantee in `docs/safety.md`. A change that touches
  placement, verification or the ffmpeg guard flags needs a test.
- Findings get a fixed ASCII code, a severity and a one-line message that
  makes sense without the code. Add new codes to `docs/report.md`.
- Profile keys are documented in `docs/profiles.md` in the same PR.
- Fixtures are generated (`testdata/gen-fixtures.sh`), never committed as
  binaries.

## Commit messages

Imperative mood, one change per commit, reference the issue when there is
one. `feat:`, `fix:`, `docs:`, `test:`, `chore:` prefixes are welcome but not
enforced.

## Code of conduct

See CODE_OF_CONDUCT.md.
