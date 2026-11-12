# Theseus

Theseus deploys prebuilt applications on a single Linux VM. GitHub Actions builds the app, then sends a release over SSH. Theseus activates the release through Caddy. exe.dev provides the HTTPS front door.

## Set up the VM

The VM needs Theseus and Caddy, plus Docker for container apps. CI deployments need no Git checkout or project build tools on the VM. To build Theseus from source, use Go 1.24 or later:

```sh
go build -o theseus ./cmd/theseus
```

1. Put the binary on the deployment user's noninteractive SSH `PATH`.
2. Run `theseus init --trusted-proxy PROXY_IP/32`, using the verified source IP of exe.dev's ingress. Use `/128` for IPv6; repeat the flag for multiple proxy ranges. Then start Caddy with the command it prints, as the same user. Theseus replaces that dedicated Caddy instance's routing through its default local administration endpoint.
3. Configure exe.dev to route to port **8000**. Set up DNS and register each custom domain with exe.dev. Choose public or private sharing explicitly.

Theseus does not install dependencies or services. Configure Caddy supervision separately for reboot persistence. State defaults to `~/.local/share/theseus`; `--home` selects another directory.

Trusted proxies preserve exe.dev's HTTPS scheme and forwarded client information. Set only the actual ingress ranges; exe.dev does not document a fixed source range. Direct local HTTP needs no `--trusted-proxy`. Existing installations can edit `ingress.toml` in the state directory (`trusted_proxies = ["PROXY_IP/32"]`); the next activation uses it. Caddy 2.8 or later is required for strict, right-to-left forwarded-address parsing.

## Connect GitHub Actions

Create a dedicated SSH key and register its public key for the target VM:

```sh
cat theseus-ci.pub | ssh exe.dev ssh-key add --vm=YOUR_VM
```

Add these settings to your application repository:

| Setting | Type | Value |
| --- | --- | --- |
| `THESEUS_SSH_KEY` | Actions secret | Private SSH key |
| `THESEUS_KNOWN_HOSTS` | Actions secret | Known-hosts entry verified against [exe.dev's published host key](https://exe.dev/docs/faq/host-key.md) |
| `THESEUS_SSH_DEST` | Actions variable | Exact VM SSH destination, including username if present |

The [VM-scoped key](https://exe.dev/docs/vm-scoped-ssh-keys.md) grants SSH access to the VM, not just deployment access. Deployment works even when the website is private.

### Static sites

Add `theseus.toml` to your app:

```toml
name = "rensberger"
branch = "main"
domains = ["rensberger.com", "www.rensberger.com"]

[static]
```

1. Copy [static.yml](examples/github-actions/static.yml) to your app's `.github/workflows/deploy.yml`.
2. Copy [theseus-deploy.sh](examples/github-actions/theseus-deploy.sh) to `.github/scripts/theseus-deploy.sh`.
3. Adjust the workflow's build commands and `dist` output directory. The example uses Bun. Add your test command before packaging.
4. Push to `main`.

Actions streams a tar.gz bundle containing `theseus.toml` and `public/` to `theseus receive` over SSH. Theseus publishes the files and reloads Caddy. Build instructions belong in the workflow.

### Containers

Use this runtime configuration instead:

```toml
name = "api"
domains = ["api.example.com"]

[docker]
port = 3000
healthcheck = "/health"
```

Copy [container.yml](examples/github-actions/container.yml) to `.github/workflows/deploy.yml` and the SSH script as above. Match the workflow's `linux/amd64` platform to your VM. For private GHCR images, configure Docker pull credentials on the VM; the runner's `GITHUB_TOKEN` only authenticates its push.

Actions publishes to GHCR and sends the immutable image digest with the configuration bundle. Theseus pulls the image, starts a candidate, checks HTTP health, and switches Caddy. Health checks require a 200–399 response within 30 seconds; the default path is `/`. Failed builds, pulls, or health checks leave the current release serving.

## Manage deployments

```sh
theseus status                    # Current and previous releases as JSON
theseus logs RELEASE_ID           # Deployment output; ID printed during deployment
theseus rollback rensberger       # Switch to the previous release
theseus recover                   # Finish an interrupted activation after fixing Caddy
theseus recover --abort           # Restore routing saved before the interrupted activation
```

Build logs live in Actions. Local deployment logs record success or the terminal failure in `build.log`; container application logs are available through `docker logs`. Recovery checks only changed container releases. Use `recover --abort` if a candidate cannot become healthy. Interrupted aborts resume on the next `recover`; older journals without a saved previous state must be completed before rollback.

Use one stable Actions workflow per project. Theseus records its repository, commit, and `github.run_number`, rejecting older or repeated numbers even after rollback. Retry failed pre-activation runs normally; use a new `workflow_dispatch` run to redeploy a successful commit. Replacing the workflow with a lower run counter requires manual state migration.

Deployments take a VM-wide lock. A simultaneous delivery from another repository fails with a lock error and needs a retry; there is no deployment queue.

## Develop locally

```sh
go test -race ./...
go vet ./...
go build -o theseus ./cmd/theseus
./theseus --home "$PWD/.theseus" init
caddy run --config "$PWD/.theseus/Caddyfile" --adapter caddyfile
```

In another terminal, deploy a committed repository using the [static](examples/static/theseus.toml) or [Dockerfile](examples/docker/theseus.toml) example:

```sh
./theseus --home "$PWD/.theseus" deploy /absolute/path/to/app
curl -H 'Host: www.example.com' http://localhost:8000/
```

The optional `deploy` command builds on the VM and requires Git, plus Docker for builds. It accepts Git URLs, including exe.dev GitHub integration URLs. The branch defaults to `main`; use `--branch` to change it. Build paths must stay within the checkout. CI-managed projects use `receive` or `rollback` instead.

## Current limits

- Bundles accept only `theseus.toml` and `public/`, with a 1 GiB expanded-size limit and 100,000 entries. Links, special files, duplicate files, and escaping paths are rejected.
- Static publication excludes `.git`, `theseus.toml`, and names starting with `.env`. Package only files intended for publication.
- Old containers remain running for rollback; images and release directories accumulate. Cleanup is manual.
- Environment-secret management, persistent-volume configuration, analytics, access-log aggregation, and systemd installation are not implemented.

This is an initial implementation for local development, not unattended operation.
