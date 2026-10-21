# Changelog

Versioned as `release.feature.fix.patch`.

## [0.5.0.0] - 2026-10-21

### Added
- Agent updates from the Machines page: **Update agent** on any machine
  whose agent is behind the server, and **Update all agents**. The agent
  checks the new version, runs it once to make sure it starts on that
  machine, swaps it in and restarts, keeping the old one as `.old`. It
  waits if a job is running. Recorded in the audit log.
- Agents only accept updates signed with The Repo Depot's release key
  (Ed25519), and never an older version. `scripts/build.sh` signs them
  (`cmd/signagents`, `dist/agents/manifest.json`).
- Agents older than 0.5.0.0 show why they can't update from here, with the
  command that updates them (once, by hand).

## [0.4.0.1] - 2026-10-20

### Fixed
- On phones the page heading no longer gets squeezed, with its buttons
  covering the subtitle; the buttons wrap below it.

## [0.4.0.0] - 2026-10-20

### Added
- Update commands for tracked apps: give a "Track only" app a shell command
  (run in its folder on its machine, with the new version in
  $REPODEPOT_REF) and The Repo Depot can update it itself — Update, Update
  selected, Update all and automatic updates. It always updates to the latest
  version; a failed command leaves the old version recorded. Needs agent
  0.4.0.0 on that machine.

### Fixed
- My Depot cards grow to show everything instead of cutting off text: app
  name, repo, folder, versions and the "Checked …" line, which now sits on
  its own line above the buttons. The Update ribbon no longer covers the logo.
- "Update selected" explains (on hover) why it's off, such as a tracked app
  without an update command.

## [0.3.0.1] - 2026-10-20

### Fixed
- Windows agent stopped straight after starting: Windows PowerShell 5.1 saved
  its config with a UTF-8 byte-order mark, which the agent couldn't read. The
  agent now accepts it and the installer no longer writes it.
- The Windows installer now checks the agent service really started instead
  of always saying it's running.
- Windows firewall rule now applies on any network type (home networks are
  often marked Public), limited to the local subnet. Re-running the installer
  fixes an older rule.

## [0.3.0.0] - 2026-10-19

### Added
- Users: invite people by email from Settings → Users as an admin, member
  (installs, updates and removes apps) or viewer (looks only). Without a mail
  server the invite link is shown to send yourself. Change roles, resend or
  cancel invites, remove people (they're signed out at once).
- Email: Settings → Email for your mail server, with a test button. Used for
  invites, "Forgot your password?" (one-hour links), and optionally
  notifications.
- Webhook notifications in Discord, Slack or plain JSON format.
- What's new: an app's release notes (or commit list) between the installed
  and latest version, before you update.
- Versions: install any recent release, tag or commit, newer or older.
  Roll back to the previous version in one click; rolling back skips the
  newer version so it isn't put straight back.
- Skip a version, hold an app's updates, ignore versions matching a pattern,
  and choose pre-releases per app.
- Auto-updates can be limited to set hours (e.g. 2 to 6 at night), in the
  time zone set in Settings → General (Docker containers run on UTC).
- Release downloads are checked against GitHub's SHA-256 digest or the
  release's checksums file; a mismatch is refused and nothing changes.
- Discover → Your stars: the repos you've starred on GitHub.
- Activity → Audit log (who changed users, machines and settings), and who
  started each job.
- Backups: a daily copy of the database (kept 7 days by default) and a
  download button in Settings → Backups.
- Machines show when their agent is older than the server.

### Changed
- An automatic update that fails is tried once per version, not again at
  every check.
- The agent install script only uses the saved public address or a
  well-formed host name.
- A GitHub "forbidden" answer that isn't a rate limit shows GitHub's reason.
- One-tap update links respect a hold or skip made after they were sent.
- Built with Go 1.27.3 (12 standard library security fixes) on Alpine 3.24.

### Fixed
- Changing your email to one another account uses said "saved" but did nothing.
- "Update selected" offered tracked-only apps.
- An app with no jobs showed a blank "undefined" job on its page.
- My Depot's "Updates available" count left out tracked-only apps (the
  sidebar counted them).

## [0.2.0.0] - 2026-10-19

### Added
- Track only: watch software that was installed some other way. You give
  the version installed now and where it is; My Depot shows it, checks for
  updates and notifies you, but never touches it. After updating it yourself,
  press Mark updated.
- An optional web address per app adds an Open button.
- Find logo again on an app's page.

### Changed
- Logos are found anywhere in the repo (one request for the whole file
  list), not only in the top folders, skipping vendored and test folders.
- A logo lookup that hits GitHub's rate limit is retried next time instead
  of falling back to the owner's picture for a week.

### Fixed
- Tracking tags picked GitHub's first-listed tag, not the newest version
  (v0.9.3.0 could win over v0.13.0.0). Tags are now compared as versions.

## [0.1.0.0] - 2026-10-19

First version.

- Discover: GitHub search with caching, keyboard navigation, paste-a-link,
  repo pages with README, releases, detected install methods and logo.
- Install on one or more machines with Docker Compose (including a generated
  compose file for Dockerfile-only repos, overrides and `.env`), release
  downloads (per-machine file choice, archives, single programs, .deb, .rpm,
  .msi, PATH link, service) or source (with a build command).
- My Depot: cards or list, filters, update one/selected/all, live progress.
- Update tracking by releases, tags or commits; scheduled checks with ETags;
  auto-update per app.
- ntfy notifications with a signed one-tap "Update now" link.
- Uninstall (optionally with data) and unregister.
- Machines with one-line agent installers for Linux (systemd) and Windows
  (service), live status checks.
- Activity log of every job; Settings page for every setting; account page.
