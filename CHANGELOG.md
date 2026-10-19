# Changelog

Versioned as `release.feature.fix.patch`.

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
