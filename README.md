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
  Optional auto-update per app, limited to set hours if you like.
- **In control of every update:** see what's new (release notes or the commit
  list) before updating, skip a version, hold an app, ignore versions matching
  a pattern, choose pre-releases per app, install any older version, or roll
  back to the previous one in one click.
- **Checked downloads:** release files are verified against GitHub's SHA-256
  digest or the release's checksum file before anything is installed.
- **Notifications** via [ntfy](https://ntfy.sh), email, or a webhook
  (Discord, Slack or JSON): "Update for X", with an **Update now** button
  that works from your phone (a signed one-update link that expires after 7 days).
- **Users:** invite people by email as admins, members (install and update)
  or viewers (look only). Forgotten passwords are reset by email. An audit log
  shows who changed what.
- **Your stars:** browse the repos you've starred on GitHub and install from there.
- **Backups:** a daily copy of the database, kept for as many days as you choose,
  and a download button.
- **Uninstall** removes exactly what was installed (recorded in
  `.repodepot.json`), optionally with its data. **Unregister** stops tracking
  without touching the machine.
- **Every setting** is editable in Settings; nothing needs a config file.

Not affiliated with or endorsed by GitHub, Inc.

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
can give it jobs.

### Updating agents

Agents update themselves: after each machine check, the server sends any
agent that's behind it the new version (Settings → Updates → Update agents
automatically, on by default). Each version is tried once per machine, and a
failure is notified. The Machines page also has **Update agent** and
**Update all agents** buttons, for admins. The server
sends the agent its new version; the agent checks it, runs it once to make
sure it starts on that machine, swaps it in and restarts, keeping the old
one beside it as `.old`. It refuses while it's in the middle of a job.

Agents only install an update signed with The Repo Depot's release key
(Ed25519; the public half is built into the agent, the private half never
leaves the build machine), and never an older version. So a machine's token
alone can't make an agent run a program of someone's choosing.

Agents older than 0.5.0.0 can't update themselves: run the install command
once more on each (no token needed, it keeps its settings; the Machines page
shows it). Running the install command again always updates the agent too.

On Windows with Smart App Control on, Windows may block a new, unsigned
agent build. The agent then stays as it is and the Machines page says why.

## Build

```sh
scripts/build.sh                 # server, agents for linux amd64/arm64 and windows amd64 (signed), Docker context in dist/
docker build -t repodepot dist/
go test ./...
```

`scripts/build.sh` signs the agents with the key in `../RepoDepot-signing/`
(or `$REPODEPOT_SIGNING_KEY_FILE` / `$REPODEPOT_SIGNING_KEY`, a base64
Ed25519 seed), writing `dist/agents/manifest.json`. Without a key the agents
are listed unsigned: they install, but can't be pushed as updates. A build
with its own key puts its public half in `UpdatePublicKey`
(`internal/agent/selfupdate.go`).

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

## License

The Repo Depot is open source under the [Apache License 2.0](LICENSE).
Copyright 2026 AC3 Labs.

You're free to use, change, fork and redistribute it, including
commercially. Forks and modified versions must keep the [NOTICE](NOTICE)
file, which credits AC3 Labs as the original creator, and must say that
they changed the files. The licence doesn't let you use the AC3 Labs or
The Repo Depot names to suggest that AC3 Labs made or endorses your
version.

**Use at your own risk.** The Repo Depot installs programs and runs
commands on the machines you connect to it. It comes with no warranty
(see LICENSE, sections 7 and 8).
