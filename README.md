# dockershrink

`dockershrink` is a CLI that inspects a Docker/OCI image and gives concrete, actionable recommendations for making it smaller, cleaner, and safer. Point it at an image and it tells you exactly *why* it's big — "layer 4 carries 180 MB of apt cache", "build toolchain is shipped in the final image, multi-stage would cut ~400 MB", "base image is pinned to a tag that's 7 months old" — instead of a vague total size.

Every check is an isolated analyzer, so the report is specific and the fixes are obvious.

## Features

- **Flexible image sources** — analyze images from the local Docker daemon, a `docker save` tarball / OCI layout, or pull directly from a registry by tag or digest.
- **Size analysis** — per-layer and per-file breakdown that attributes bloat to the exact instruction that caused it: leftover package-manager caches (apt/apk/yum/pip/npm), files deleted in a later layer but still occupying earlier ones, duplicates, temp junk, docs, and combinable `RUN` steps.
- **Best-practice checks** — missing multi-stage builds, build tools left in runtime images, unpinned/`:latest` base tags, stale base images, full vs. slim/distroless variants, `ADD` vs `COPY`, running as root, missing `HEALTHCHECK`.
- **Supply-chain hygiene** — scan your own images for known CVEs in OS packages (OSV-backed), outdated packages, accidentally baked-in secrets, and a SUID/SGID inventory.
- **Reporting & CI** — estimated savings per finding, a single bloat score, image-to-image diff, baseline comparison, `json`/SARIF output, and a `--fail-on` mode for pipelines.

## Installation

```sh
# from a release
curl -L https://github.com/<user>/dockershrink/releases/latest/download/dockershrink_linux_amd64 -o /usr/local/bin/dockershrink
chmod +x /usr/local/bin/dockershrink

# or from source
go install github.com/<user>/dockershrink@latest
```

## Quick start

```sh
# analyze a local image
dockershrink analyze myapp:latest

# pull and analyze straight from a registry
dockershrink analyze registry.example.com/team/myapp@sha256:abc123...

# machine-readable output for CI, fail the build on critical findings
dockershrink analyze myapp:latest -o json --fail-on critical

# compare two builds
dockershrink diff myapp:1.4 myapp:1.5
```

Example output:

```
myapp:latest  →  412 MB  (bloat score: 38/100)

CRITICAL  layer 6   180 MB apt cache not cleaned (/var/lib/apt/lists)
                    fix: add `rm -rf /var/lib/apt/lists/*` to the same RUN
WARN      config    build toolchain (gcc, make) present in final image
                    fix: split into a multi-stage build  (~210 MB)
WARN      base      base image debian:bullseye is ~7 months behind latest
INFO      layer 3   42 MB of man pages and /usr/share/doc

Estimated savings: ~390 MB (95%)
```

## Status

Under active development. Expect breaking changes before `v1.0`.

## License

MIT
