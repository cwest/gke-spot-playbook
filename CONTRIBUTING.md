<!-- repo:managed -->
# How to Contribute

We would love to accept your patches and contributions to this project.

## Before you begin

### Sign our Contributor License Agreement

Contributions to this project must be accompanied by a
[Contributor License Agreement](https://cla.developers.google.com/about) (CLA).
You (or your employer) retain the copyright to your contribution; this simply
gives us permission to use and redistribute your contributions as part of the
project.

If you or your current employer have already signed the Google CLA (even if it
was for a different project), you probably don't need to do it again.

Visit <https://cla.developers.google.com/> to see your current agreements or to
sign a new one.

### Review our Community Guidelines

This project follows [Google's Open Source Community
Guidelines](https://opensource.google/conduct/).

## Contribution process

### Development setup

Install the toolchain, then run the suite:

```bash
make tools   # installs the pinned kubeconform via `go install`
make test    # Go tests, pytest, shellcheck, and manifest validation
```

`make tools` needs `go` (1.26.5+) on your PATH and installs into
`$(go env GOPATH)/bin`, so make sure that directory is on your PATH. On macOS or
Linuxbrew, `brew install kubeconform shellcheck` works just as well. You also
need `python3` for the worker test suites.

`make test` runs `check-tools` first, so a missing prerequisite reports which
tool is absent and how to install it rather than failing with a bare
`command not found`.

### Code Reviews

All submissions, including submissions by project members, require review. We
use [GitHub pull requests](https://docs.github.com/articles/about-pull-requests)
for this purpose.

Every pull request runs the full suite in CI, which validates against the same
pinned tool versions your local `make test` uses.
