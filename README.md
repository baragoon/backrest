<!-- markdownlint-disable MD033 MD041 -->

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="./webui/assets/logo.svg" width="400px">
    <source media="(prefers-color-scheme: light)" srcset="./webui/assets/logo-black.svg" width="400px">
    <img src="./webui/assets/logo.svg" width="400px" alt="Backrest logo">
  </picture>
</p>

<p align="center">
  <img src="https://github.com/baragoon/backrest/actions/workflows/test.yml/badge.svg" alt="Test workflow status" />
  <img src="https://img.shields.io/github/downloads/baragoon/backrest/total" alt="Total GitHub downloads" />
</p>

---

## Overview

Backrest is a web-accessible backup solution built on top of [restic](https://restic.net/). Backrest provides a WebUI which wraps the restic CLI and makes it easy to create repos, browse snapshots, and restore files. Additionally, Backrest can run in the background and take an opinionated approach to scheduling snapshots and orchestrating repo health operations.

By building on restic, Backrest leverages its mature, fast, reliable, and secure backup capabilities while adding an intuitive interface.

Built with Go, Backrest is distributed as a standalone, lightweight binary with restic as its sole dependency. It can securely create new repositories or manage existing ones. Once storage is configured, the WebUI handles most operations, while still allowing direct access to the powerful [restic CLI](https://restic.readthedocs.io/en/latest/manual_rest.html) for advanced operations when needed.

## Key Features

- **Web Interface**: Access locally or remotely (perfect for NAS deployments)
- **Multi-Platform Support**:
  - Linux
  - macOS
  - Windows
  - FreeBSD
  - [Docker](https://github.com/baragoon/backrest/pkgs/container/backrest)
  - [Docker](https://github.com/baragoon/backrest/pkgs/container/backrest)
- **Backup Management**:
  - Import existing restic repositories
  - Cron-scheduled backups and maintenance (e.g. prune, check, forget, etc)
  - Browse and restore files from snapshots
  - Configurable notifications (Discord, Slack, Shoutrrr, Gotify, Healthchecks)
  - Pre/post backup command hooks to execute shell scripts
- **Storage Options**:
  - Compatible with rclone remotes
  - Supports all restic storage backends (S3, B2, Azure, GCS, local, SFTP, and [all rclone remotes](https://rclone.org/))

## Preview

<p align="center">
   <img src="https://f000.backblazeb2.com/file/gshare/screenshots/backrest-1.11.1-dashboard.png" width="80%" alt="Backrest dashboard" />
   <img src="https://f000.backblazeb2.com/file/gshare/screenshots/backrest-1.11.1-browse-snapshot.png" width="80%" alt="Browse a snapshot" />
   <img src="https://f000.backblazeb2.com/file/gshare/screenshots/backrest-1.11.1-add-plan.png" width="80%" alt="Create a backup plan" />
</p>

---

# User Guide

[See the Backrest docs](https://garethgeorge.github.io/backrest/introduction/getting-started).

---

# Installation

Backrest is packaged as a single executable. It runs directly on Linux, macOS, and Windows. [restic](https://github.com/restic/restic) is downloaded automatically on first run.

Once installed, access Backrest at `http://localhost:9898` (default port). First-time setup will prompt for username and password creation.

> [!NOTE]
> To change the default port, set the `BACKREST_PORT` environment variable (e.g., `BACKREST_PORT=0.0.0.0:9898` to listen on all interfaces). The install script accepts `--allow-remote-access` as a shortcut for this.
>
> Backrest will use your system's installed version of restic if it's available and compatible. If not, Backrest will download and install a suitable version in its data directory, keeping it updated. On Unix, if an update fails, Backrest will warn and use an existing runnable managed binary, which may not be supported. To use a specific restic binary, set the `BACKREST_RESTIC_COMMAND` environment variable to the desired path.

## Linux & macOS (Recommended)

The install script downloads the latest release, drops the binary into `/usr/local/bin`, and sets up the appropriate auto-start integration (systemd or OpenRC on Linux; launchd on macOS):

```sh
curl -fsSL https://raw.githubusercontent.com/baragoon/backrest/main/install.sh | bash
```

Flags go after `--`:

```sh
# Bind to all interfaces (default: 127.0.0.1:9898)
curl -fsSL https://raw.githubusercontent.com/baragoon/backrest/main/install.sh | bash -s -- --allow-remote-access

# Uninstall (removes service, autostart entry, and /usr/local/bin/backrest)
curl -fsSL https://raw.githubusercontent.com/baragoon/backrest/main/install.sh | bash -s -- --uninstall
```

The service runs as your user by default (so config and data live under your `$HOME`). To install as `root` instead, pass `--root`. After install, access Backrest at `http://localhost:9898`.

> [!TIP]
> Review [install.sh](./install.sh) before piping it into a shell. You can also clone the repo and run `./install.sh` locally; it accepts the same flags.

### Arch Linux (AUR)

[Backrest on AUR](https://aur.archlinux.org/packages/backrest) is third-party (not maintained by the Backrest project) and tweaks the systemd unit; see the [AUR service file](https://aur.archlinux.org/cgit/aur.git/tree/backrest@.service?h=backrest) for details.

```sh
paru -Sy backrest  # or: yay -Sy backrest
sudo systemctl enable --now backrest@$USER.service
```

## Docker

Image: `ghcr.io/baragoon/backrest`.

- Includes rclone and common Unix utilities
- For a minimal image, use `ghcr.io/baragoon/backrest:scratch`

### Docker Compose

```yaml
version: "3.8"
services:
  backrest:
    image: ghcr.io/baragoon/backrest:latest
    container_name: backrest
    hostname: backrest
    volumes:
      - ./backrest/data:/data
      - ./backrest/config:/config
      - ./backrest/cache:/cache
      - ./backrest/tmp:/tmp
      - ./backrest/rclone:/root/.config/rclone # Mount for rclone config (needed when using rclone remotes)
      - /path/to/backup/data:/userdata  # Mount local paths to backup
      - /path/to/local/repos:/repos     # Mount local repos (optional for remote storage)
    environment:
      - BACKREST_DATA=/data
      - BACKREST_CONFIG=/config/config.json
      - XDG_CACHE_HOME=/cache
      - TMPDIR=/tmp
      - TZ=America/Los_Angeles
    ports:
      - "9898:9898"
    restart: unless-stopped
```

### Migrating from Docker Hub

Docker Hub images no longer receive updates. In your Compose file, change
`baragoon/backrest` to `ghcr.io/baragoon/backrest`, keeping your existing tag
(for example, `latest` or `scratch`). Ensure your configuration and data are
stored in bind mounts or volumes, then run:

```sh
docker compose pull backrest
docker compose up -d --no-deps --force-recreate backrest
```

These commands recreate the container without removing its volumes.

### Migrating from Docker Hub

Docker Hub images no longer receive updates. In your Compose file, change
`baragoon/backrest` to `ghcr.io/baragoon/backrest`, keeping your existing tag
(for example, `latest` or `scratch`). Ensure your configuration and data are
stored in bind mounts or volumes, then run:

```sh
docker compose pull backrest
docker compose up -d --no-deps --force-recreate backrest
```

These commands recreate the container without removing its volumes.

## Windows

Download the Windows installer for your architecture from the [releases page](https://github.com/baragoon/backrest/releases). The installer, named `Backrest-setup-[arch].exe`, places Backrest and a GUI tray application in `%localappdata%\Programs\Backrest\`. The tray application, set to start on login, monitors Backrest.

> [!TIP]
> To override the default port before installation, set a user environment variable named `BACKREST_PORT`. On Windows 10+, navigate to Settings > About > Advanced system settings > Environment Variables. Under "User variables", create a new variable `BACKREST_PORT` with the value `127.0.0.1:port` (e.g. `127.0.0.1:8080`). If changing post-installation, re-run the installer to update shortcuts with the new port.

---

# Configuration

## Environment Variables (Unix)

| Variable                  | Description                 | Default                                                                                                             |
| ------------------------- | --------------------------- | ------------------------------------------------------------------------------------------------------------------- |
| `BACKREST_PORT`           | Port to bind to             | 127.0.0.1:9898 (or 0.0.0.0:9898 for the docker images)                                                              |
| `BACKREST_CONFIG`         | Path to config file         | `$HOME/.config/backrest/config.json`<br>(or, if `$XDG_CONFIG_HOME` is set, `$XDG_CONFIG_HOME/backrest/config.json`) |
| `BACKREST_DATA`           | Path to the data directory  | `$HOME/.local/share/backrest`<br>(or, if `$XDG_DATA_HOME` is set, `$XDG_DATA_HOME/backrest`)                        |
| `BACKREST_RESTIC_COMMAND` | Path to restic binary       | Defaults to a Backrest managed version of restic at `$XDG_DATA_HOME/backrest/restic-x.x.x`                          |
| `XDG_CACHE_HOME`          | Path to the cache directory |                                                                                                                     |

## Environment Variables (Windows)

| Variable                  | Description                 | Default                                                                                    |
| ------------------------- | --------------------------- | ------------------------------------------------------------------------------------------ |
| `BACKREST_PORT`           | Port to bind to             | 127.0.0.1:9898                                                                             |
| `BACKREST_CONFIG`         | Path to config file         | `%appdata%\backrest\config.json`                                                           |
| `BACKREST_DATA`           | Path to the data directory  | `%appdata%\backrest\data`                                                                  |
| `BACKREST_RESTIC_COMMAND` | Path to restic binary       | Defaults to a Backrest managed version of restic in `C:\Program Files\restic\restic-x.x.x` |
| `XDG_CACHE_HOME`          | Path to the cache directory |                                                                                            |

<!-- coderabbit-ai-generated:v1 -->

<!-- coderabbit-ai-generated-section:v1:e592cb18e59f910199df5f4752813bf29a7358024ee334dd3b3b1f5a58604f69 -->

## Healthcheck address file locking

Backrest stores healthcheck locks in a private subdirectory of its data directory on Linux and macOS, and in the user profile on Windows. Set `BACKREST_HEALTH_BIND_ADDRESS_LOCK_FILE` to use an existing operator controlled directory instead. If server processes from different accounts or with different data directories share an explicit `BACKREST_HEALTH_BIND_ADDRESS_FILE`, configure them with the same lock path and give each account read and write access to the regular lock file.

# Development


<!-- /coderabbit-ai-generated:v1 -->
## Contributing

Contributions are welcome! See the [issues](https://github.com/baragoon/backrest/issues) or feel free to open a new issue to discuss a project. Beyond the core codebase, contributions to [documentation](https://garethgeorge.github.io/backrest/introduction/getting-started), [cookbooks](https://garethgeorge.github.io/backrest/cookbooks/command-hook-examples), and testing are always welcome.

## Build Dependencies

All build dependencies are defined in `shell.nix` and can be activated automatically using [Nix](https://nixos.org/) and [direnv](https://direnv.net/).

### Using Nix + direnv (Recommended)

1. Install [Nix](https://nixos.org/download/) and [direnv](https://direnv.net/docs/installation.html)
2. [Hook direnv into your shell](https://direnv.net/docs/hook.html) (e.g. `eval "$(direnv hook bash)"` in your `.bashrc`)
3. Clone the repo and `cd` into it
4. Run `direnv allow` to trust the `.envrc` — all dependencies (Go, Node.js, pnpm, protoc, buf, etc.) will be available in your shell automatically

### Manual Setup

If you prefer not to use Nix, install the following manually:

- [Go](https://go.dev/) 1.24 or greater
<!-- coderabbit-ai-generated:v1 -->

<!-- coderabbit-ai-generated-section:v1:8de5c9990ffccb7230da2648aa1f2dd5603f4bd704f29bfb553b138129f9e9cd -->

- [Node.js](https://nodejs.org/en) 22.13.0 or greater and [pnpm](https://pnpm.io/) 11

<!-- /coderabbit-ai-generated:v1 -->
- [goreleaser](https://github.com/goreleaser/goreleaser) `go install github.com/goreleaser/goreleaser/v2@latest`

**(Optional) To edit protobuf definitions:**

```sh
apt install -y protobuf-compiler
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
go install github.com/bufbuild/buf/cmd/buf@latest
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install connectrpc.com/connect/cmd/protoc-gen-connect-go@latest
npm install -g @bufbuild/protoc-gen-es
```

## Compiling

```sh
(cd webui && pnpm i && pnpm run build)
(cd cmd/backrest && go build .)
```

## Using VSCode Dev Containers

The dev container uses Nix and direnv to provide all dependencies. When the container starts, `direnv allow` runs automatically so the Nix shell is activated in every terminal.

0. Make sure Docker and VSCode with the [Dev Containers](https://marketplace.visualstudio.com/items?itemName=ms-vscode-remote.remote-containers) extension is installed
1. Clone this repository
2. Open this folder in VSCode
3. When prompted, click on `Open in Container` button, or run `> Dev Containers: Rebuild and Reopen in Containers` command
4. When the container is started, go to `Run and Debug`, choose `Debug Backrest (backend+frontend)` and run it

> [!NOTE]
> Provided launch configuration has hot reload for the typescript frontend.

## Translations

Translations are stored in [./webui/messages](./webui/messages) and are generated using [inlang](https://inlang.com/). Machine translations can be updated by running `npx @inlang/cli machine translate --project ./project.inlang`.

Text is translated on a best-effort basis and is not guaranteed to be accurate. If you find any translations that are incorrect, please submit a pull request to fix them. Contributions here are greatly appreciated!
