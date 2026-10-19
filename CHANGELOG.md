# Changelog

Versioned as `release.feature.fix.patch`.

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
