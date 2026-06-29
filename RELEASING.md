# Releasing

This repository publishes versioned binaries to GitHub Releases when a tag is pushed.
It also builds and publishes a Docker image to GitHub Container Registry (GHCR).

## What gets published

For each tag like `v1.2.3`, GitHub Actions builds release archives for:

- `linux/amd64`
- `linux/arm64`
- `darwin/amd64`
- `darwin/arm64`
- `windows/amd64`
- `windows/arm64`

Each archive includes:

- the `epusdt` binary
- `src/.env.example`
- `src/static/`

Windows artifacts are published as `.zip`. Other platforms are published as `.tar.gz`.

Each release also publishes:

- a `SHA256SUMS` checksum file
- a GitHub build provenance attestation for the checksum file

## How to cut a release

1. Make sure the release commit has already landed on the default branch.
2. Create an annotated tag:

```bash
git tag -a v1.0.0 -m "release v1.0.0"
```

3. Push the tag:

```bash
git push origin v1.0.0
```

4. Wait for the `release` workflow to finish on GitHub.
5. Wait for the `Docker Image` workflow to finish on GitHub if you need a container image.
6. Download the generated binaries from the GitHub Release page, or deploy the GHCR image.

## Docker image

Pushing a tag like `v1.2.3` publishes:

- `ghcr.io/xianyvbang/epusdt:1.2.3`
- `ghcr.io/xianyvbang/epusdt:1.2`
- `ghcr.io/xianyvbang/epusdt:1`
- `ghcr.io/xianyvbang/epusdt:v1.2.3`
- `ghcr.io/xianyvbang/epusdt:latest`
- `ghcr.io/xianyvbang/epusdt:sha-<commit>`

For example, after pushing `v1.2.3`, update the production compose file:

```yaml
services:
  epusdt:
    image: ghcr.io/xianyvbang/epusdt:1.2.3
    restart: always
    network_mode: host
    environment:
      EPUSDT_CONFIG: /data/.env
    volumes:
      - ./data:/data
```

Then replace the running container:

```bash
docker compose pull epusdt
docker compose up -d --no-deps --force-recreate epusdt
docker compose logs -f --tail=100 epusdt
```

If the GHCR package is private, log in on the production server first:

```bash
echo "<github-token>" | docker login ghcr.io -u <github-username> --password-stdin
```

## Verify a release

Verify the GitHub build provenance attestation:

```bash
gh attestation verify SHA256SUMS -R GMwalletApp/epusdt
```

## Local validation

Run a snapshot build locally before pushing a tag:

```bash
cd src
go run github.com/goreleaser/goreleaser/v2@latest release --snapshot --clean
```

The resulting artifacts will be generated under `src/dist/`.

## Release candidates

If you use tags like `v1.2.3-rc1`, GoReleaser will mark the GitHub release as a prerelease.

For a final tag like `v1.2.3`, the release workflow forces GoReleaser to compare against the previous stable tag instead of the latest `rc` tag. This keeps the final changelog focused on stable-to-stable changes.
