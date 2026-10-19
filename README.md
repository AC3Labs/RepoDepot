# The Repo Depot

Find software on GitHub, install it on any machine on your network, and keep
it updated, from one fast web app.

- **Discover:** search all of GitHub (or paste a link), read the README,
  releases and what's needed to install, then press **Install**.
- **Install anywhere:** pick one or more machines. Each runs a small agent
  (Linux or Windows) that does the installing.
- **Three ways to install**, detected per repo:
  - **Docker Compose:** the project's own compose file (or one written for a
    Dockerfile-only repo), with your changes kept in an override file and `.env`.
  - **Release download:** the right file for each machine's OS and CPU is
    picked from the release; archives, single programs, `.deb`, `.rpm` and `.msi`.
    Optionally put on the PATH or run as a service.
  - **Source:** the code at a tag or commit, plus your build command.
- **My Depot:** every install with its logo, machine, folder and version.
  Update one, the selected ones, or all.
- **Updates:** track releases, tags, or every commit on a branch. Checks run on
  a schedule; unchanged repos cost nothing against GitHub's rate limit (ETags).
  Optional auto-update per app.
- **Notifications** via [ntfy](https://ntfy.sh): "Update for X", with an
  **Update now** button that works from your phone (a signed one-update link
  that expires after 7 days).
- **Uninstall** removes exactly what was installed (recorded in
  `.repodepot.json`), optionally with its data. **Unregister** stops tracking
  without touching the machine.
- **Every setting** is editable in Settings; nothing needs a config file.

## Run it

```sh
docker compose up -d     # http://<server>:8470
```

Open it, create your account, then **Machines → Add machine**. It gives you a
one-line command for that machine:

```sh
# Linux (systemd)
curl -fsSL http://<server>:8470/agent/install.sh | sudo sh -s -- --token <token>
```
```powershell
# Windows, in Terminal (Admin)
& ([scriptblock]::Create((irm 'http://<server>:8470/agent/install.ps1'))) -Token '<token>'
```

The agent listens on port 8471 and runs as root/Administrator, because
installing software needs it. Only the server, holding the machine's token,
can give it jobs. Running the command again updates the agent.

## Build

```sh
scripts/build.sh                 # server, agents for linux amd64/arm64 and windows amd64, Docker context in dist/
docker build -t repodepot dist/
go test ./...
```

## How it fits together

```
browser ── The Repo Depot server (Go, SQLite, :8470) ── GitHub API (cached)
                     │  jobs over HTTP (bearer token)
          ┌──────────┴──────────┐
     agent :8471           agent :8471        each downloads straight from GitHub
     (Linux)               (Windows)          and installs into its apps folder
```

Third-party code: [Alpine.js](https://alpinejs.dev) (MIT) for the page
behaviour; Go modules listed in `go.mod`.
