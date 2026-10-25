# Vendored JavaScript

Checked in rather than fetched, because the binary is the whole deployment.
A script tag pointing at a CDN means a dashboard that breaks on an air-gapped
install and a supply chain that can serve different bytes than were tested.

| File | Version | Source |
|---|---|---|
| `htmx.min.js` | 2.0.4 | https://unpkg.com/htmx.org@2.0.4/dist/htmx.min.js |
| `joint.min.js` | @joint/core 4.3.3 (MPL-2.0, `joint.LICENSE`) | https://registry.npmjs.org/@joint/core/-/core-4.3.3.tgz, `package/dist/joint.min.js` |
| `xterm.js` | @xterm/xterm 6.0.0 (MIT, `xterm.LICENSE`) | https://registry.npmjs.org/@xterm/xterm/-/xterm-6.0.0.tgz, `package/lib/xterm.js` |
| `../css/xterm.css` | @xterm/xterm 6.0.0 (MIT, `xterm.LICENSE`) | the same tarball, `package/css/xterm.css` |
| `addon-fit.js` | @xterm/addon-fit 0.11.0 (MIT, `xterm.LICENSE`) | https://registry.npmjs.org/@xterm/addon-fit/-/addon-fit-0.11.0.tgz, `package/lib/addon-fit.js` |

The xterm files are what `npm pack @xterm/xterm @xterm/addon-fit` downloads,
unchanged. The stylesheet sits with the other CSS rather than here because it
is served as a stylesheet; it is loaded only by the console tab, like the
scripts, so no other page pays for a terminal it does not draw.

To update: fetch the new version, commit it, and change the version here.
Nothing builds these — they are the file the browser receives.
