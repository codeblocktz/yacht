<!--
Small, obviously-correct fixes need none of this — a sentence is fine.
-->

## What this changes

<!-- What it does, and why that is the right change. Not a restatement of the diff. -->

## Why

<!-- The problem this solves, or the issue it closes. Fixes #123 -->

## Checks

- [ ] `make check` passes — and I looked for `SKIP`, since database tests skip
      themselves without `YACHT_TEST_DATABASE_URL` and a skip reads as a pass
- [ ] `make assets` run and the regenerated `*_templ.go` / `app.css` committed,
      if I touched a template or the CSS
- [ ] Behaviour changes come with a test; bug fixes come with one that failed
      before the fix
- [ ] New visual states added to the gallery (`internal/web/gallery_test.go`)

## Anything reviewers should know

<!-- Trade-offs you weighed, things you were unsure about, what you did not test. -->
