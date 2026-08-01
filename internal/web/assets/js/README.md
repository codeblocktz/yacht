# Vendored JavaScript

Checked in rather than fetched, because the binary is the whole deployment.
A script tag pointing at a CDN means a dashboard that breaks on an air-gapped
install and a supply chain that can serve different bytes than were tested.

| File | Version | Source |
|---|---|---|
| `htmx.min.js` | 2.0.4 | https://unpkg.com/htmx.org@2.0.4/dist/htmx.min.js |

To update: fetch the new version, commit it, and change the version here.
Nothing builds these — they are the file the browser receives.
