# UI Responsiveness and Accessibility Design

## Goal

Improve Yacht's mobile usability and keyboard accessibility without changing
the current desktop UI, visual language, density, colors, typography, or server
workflows.

## Visual Constraint

At desktop widths, the resulting interface must remain visually unchanged.
Existing tokens, panels, tables, navigation, buttons, spacing, typography, and
status treatments remain authoritative. Mobile adaptations must reuse those
same primitives rather than introduce a new look.

## Scope

### Application shell

- Add a keyboard-visible skip link targeting the main content.
- Reduce standard page horizontal padding from 24px to 16px below the `sm`
  breakpoint while retaining 24px at and above `sm`.
- Preserve the existing desktop sidebar and collapsed rail exactly.
- Make the existing mobile sidebar behave as a modal drawer: focus moves into
  it when opened, Tab and Shift+Tab remain within it, Escape and the backdrop
  close it, background content becomes inert, and focus returns to the toggle.
- Keep the current drawer dimensions, colors, animation, and backdrop.

### Responsive data surfaces

- Introduce a shared horizontal overflow wrapper for dense tables.
- Apply it to Team members, pending invitations, app variables, and app storage.
- Retain current desktop table markup and appearance.
- Allow action groups and forms to wrap or stack only where narrow widths would
  otherwise cause document-level overflow.

### Forms and controls

- Preserve the current 32px desktop control height and 28px small-button height.
- Increase primary touch targets to approximately 40px below the `sm`
  breakpoint without changing their desktop dimensions.
- Make the Team invitation form stack below `sm` and retain its horizontal
  desktop layout.
- Style the Secret checkbox with the existing Yacht tokens and a larger label
  hit area; submitted field name and value remain unchanged.

### App detail navigation

- Preserve the existing app rail at `lg` and above.
- Below `lg`, replace the potentially 420px-tall app list with a compact app
  switcher using existing form styling.
- Navigation remains ordinary full-page navigation and requires no new server
  endpoint or client-side data.

### Destructive confirmations

- Replace browser-native `confirm()` prompts with one reusable accessible
  confirmation dialog.
- Keep the existing destructive forms, methods, actions, and server validation.
- Each trigger supplies a title, consequence text, and explicit confirmation
  label. Cancel is the initial safe action; confirming submits the original
  form.
- Cover app deletion, registry removal, node drain/removal, domain removal, and
  request-logging activation. Existing typed confirmation for volume deletion
  remains unchanged because it is part of the form's server workflow.
- Match the existing panel, button, border, radius, and color system.

## Architecture

Shared behavior belongs in the application shell and shared UI components.
Responsive rules belong in `assets/css/input.css`; individual page templates
only add the structural hooks required by those rules. The confirmation dialog
is a Templ component rendered once by the layout and driven by small framework-
free JavaScript using data attributes.

No new runtime dependency is introduced. Existing Templ, Tailwind, HTMX, and
plain JavaScript conventions remain in use.

## Accessibility Behavior

- The skip link is hidden until keyboard focus.
- The mobile drawer has an accessible navigation label and maintains a correct
  `aria-expanded` value on every sidebar toggle.
- Opening the drawer focuses the first interactive control; closing restores
  the control that opened it.
- The confirmation dialog has a programmatic title and description, traps
  focus while open, closes on Escape, and restores trigger focus.
- All newly styled controls retain visible `focus-visible` treatment.

## Testing

- Add rendering tests for the skip target, responsive table wrappers, mobile
  app switcher, confirmation metadata, and responsive Team form classes.
- Add focused behavior coverage for shared scripts where the existing test
  harness supports it; otherwise assert the complete accessible markup and
  script hooks from rendered HTML.
- Regenerate Templ and CSS artifacts.
- Run TypeScript checks, Go tests, Go vet, and the visual gallery.
- Inspect representative desktop and 390px-wide pages, confirming no desktop
  visual regression and no document-level horizontal overflow.

## Out of Scope

- HTTP caching and security headers.
- Authentication or authorization changes.
- New navigation structure, branding, color, typography, or desktop spacing.
- Server API or persistence changes.
- A redesign of existing tables, panels, charts, or empty states.
