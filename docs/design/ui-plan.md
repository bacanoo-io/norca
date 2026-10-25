# Web UI and Tabler migration – plan

**Status:** In progress · 2026-10-25 (phase 1, foundation, done; phase 2, existing areas, in progress) · Starting point: the Nomad v1.6.5 web UI · Visual mockup: [mockups/ui-mockup.html](mockups/ui-mockup.html) · Related documents: [nomad-fork-development-plan.md](nomad-fork-development-plan.md) and the other design documents

The fork's UI gets a new information architecture, an overview dashboard, cross-region views, and pages for the planned features (backup, packages, objects, quotas, admission, networking, scaling, multiregion deployment). The visual base is Tabler (MIT), through our own Ember component layer. The visual mockup is the clickable realization of this plan, with real Tabler CSS.

In the mockup, the UI's working name is **Norca**. The "Nomad" name is a trademark; for public distribution the product name must be finalized together with the rebranding.

## Contents

- [Starting point](#starting-point)
- [Information architecture](#information-architecture)
- [Global frame](#global-frame)
- [Pages](#pages)
- [Common UX patterns](#common-ux-patterns)
- [Visual direction and design tokens](#visual-direction-and-design-tokens)
- [Tabler migration](#tabler-migration)
- [Schedule](#schedule)
- [Acceptance criteria](#acceptance-criteria)
- [Risks](#risks)
- [Open questions](#open-questions)

## Starting point

| Aspect | Status in v1.6.5 |
| --- | --- |
| Technology | Ember.js 3.28 (an unsupported major version), Bulma 0.9.3, partly HashiCorp Design System 2.12; 125 components, 264 template files, about 7,900 lines of custom SCSS |
| Navigation | Flat menu: Jobs, Optimize, Storage, Variables, Clients, Servers, Topology, Evaluations, Access Control (`ui/app/templates/components/gutter-menu.hbs`) |
| Landing page | None; the root redirects to the jobs list (`ui/app/routes/index.js`) |
| Regions | Region switcher (`region-switcher`), one region visible at a time |
| Live updates | Blocking-query-based adapters; the UI does not use the event stream |
| Search | Global search and a keyboard shortcut help exist |
| Visualization | Topology view, d3-based chart primitives, scaling event chart |
| Language | English only |
| Dead element | "Optimize" menu item (an Enterprise feature) |

**Main problems:** there is no overview; the menu is a flat list of nouns that the planned features would extend by another eleven-odd areas; everything starts from jobs; regions are visible only one at a time; there is no unified event stream; most modifying operations are available only from the CLI.

## Information architecture

| Group | Pages | Source |
| --- | --- | --- |
| Overview | Dashboard (per region and federated) | new |
| Workloads | Jobs, Packages, Objects, Deployments | existing; [package-management-plan.md](package-management-plan.md); K8s item 8; multiregion |
| Services and networking | Services, Intentions, Network rules | [consul-independence-plan.md](consul-independence-plan.md); K8s item 7 |
| Capacity | Clients, Node pools, Topology, Storage, Quotas, Scaling | existing; K8s item 1; autoscaler |
| Configuration | Variables, Admission policies, Type definitions | existing; K8s items 2 and 8 |
| Operations | Events and audit, Backups, Servers and regions, Evaluations | event stream; [backup-restore-plan.md](backup-restore-plan.md) |
| Access | Tokens, policies, roles, auth methods | existing |

The "Optimize" menu item is removed (Enterprise cleanup).

## Global frame

- **Top bar:** region selector with an "All regions" mode (lists and the dashboard span regions), namespace selector, command palette (Ctrl+K: navigation, search, actions), notifications from the event stream, dark mode, user and token.
- **Left menu:** with the groups above, collapsible; the group headers are not clickable.
- **Page header:** title, a short position indicator (region, namespace), the page's primary action on the right.

## Pages

| Page | Content | Backend dependency |
| --- | --- | --- |
| Dashboard | Federation strip (per region: cloud, leader, clients, allocations, last backup); health numbers; warnings (old backup, release drift, nearly full quota, failed hook, `Drifted` object, evaluation blocked by quota); deployments in progress; latest events | event stream, backup, packages, quotas |
| Jobs | List with a region column in "All regions" mode, owner (release or object), status, type | multiregion |
| Job details | Tabs: Overview, Allocations, Deployments (multiregion timeline, Run / Unblock / Fail), Versions (including those retrievable from backups), Scaling (policy editor, metric–target chart, decisions), Services (with per-instance health), Placement (affinity, state of the disruption budget), Events | autoscaler, backup, K8s items 4–6 |
| Packages | Release list; details: resources, revision history, revision comparison, rollback, hooks and tests, drift | package management |
| Objects | Per type, a table according to the definition's columns; details: spec, status, conditions, event log; editor with schema validation and dry run | K8s item 8 |
| Deployments | Cross-job list, multiregion deployments with a per-region timeline | multiregion |
| Services | Catalog with health state, per region; status of the Consul-compatible layer | Consul independence |
| Intentions and network rules | Graph: who can talk to whom; rule list; counters of dropped connections | Consul independence phase 5; K8s item 7 |
| Quotas | Usage bars per namespace and region; evaluations blocked by quota | K8s item 1 |
| Scaling | Overview of scaling policies, decision timeline, built-in or external engine | autoscaler |
| Admission policies | List; CEL editor; "try on existing jobs" (which jobs it would reject); results of warning mode | K8s item 2 |
| Type definitions | Types with schema, columns, rules | K8s item 8 |
| Backups | Schedule status, backup list, manual backup, pinning, comparison, restore wizard | backup plan |
| Events and audit | A unified, filterable stream (deployment, scaling, admission, quota, backup, release), with a link to the affected item | event stream |
| Servers and regions | Per region: servers, leader, Raft state, federation connection | existing |

The content of the existing pages (clients, node pools, topology, storage, Variables, evaluations, access) is kept, migrated to the new frame and components.

## Common UX patterns

- **Lists:** filtering, saved views, column selection, bulk actions, region column in "All regions" mode.
- **Editor:** a unified HCL and JSON editor with schema validation and completion; before every modification a preview and diff (jobs, objects, rules, releases), modeled on `job plan`.
- **Dangerous operations:** a unified confirmation; typed confirmation and two-person approval where the backend plan requires it (for example full restore).
- **Live updates:** one event stream connection per region updates all open views.
- **Empty and error states:** always say what to do (for example "No backup yet. The first scheduled backup runs at 14:00, or start one manually.").
- **Accessibility:** fully operable by keyboard, visible focus, adequate contrast, reduced motion following the system setting.
- **Multilingual:** `ember-intl`, English and Hungarian first.

## Visual direction and design tokens

**Core idea:** the fork's distinctive trait is federated operation spanning multiple clouds, so the dashboard's single emphasized element is the **federation strip**: one column per region, in the cloud's color, with the leader, the capacity and the last backup. Everything else is restrained: Tabler's neutral surfaces, with status colors used for status only.

| Token | Value | Role |
| --- | --- | --- |
| Ink | `#10233a` | Left menu, dark emphasis of the main text |
| Petrol (primary) | `#1f6f8b` | Actions, links, selection |
| Region A | `#1f6f8b` | Region identifier color |
| Region B | `#6741d9` | Region identifier color |
| Region C | `#b5651d` | Region identifier color |
| Status colors | Tabler's `success`, `warning`, `danger` colors | For status indication only |

- **Typeface:** IBM Plex Sans (text, headings), IBM Plex Mono only for code, HCL and identifiers; system font as fallback. Tabular figures for numbers.
- **Dark mode:** with Tabler's `data-bs-theme` mechanism, starting from the system setting.
- **Charts:** the existing d3 primitives aligned to the tokens; Apache ECharts or Chart.js for new charts. The mockup uses plain SVG.

## Tabler migration

### License and version

| Package | Version | License | Note |
| --- | --- | --- | --- |
| `@tabler/core` | 1.6.1 (2026-09-28) | MIT | 1.7.x is less than two weeks old; to be adopted later |
| `@tabler/icons` | 3.49.0 (2026-10-05) | MIT | Replaces the HashiCorp icon set |
| `bootstrap` | 5.3.x | MIT | The basis of Tabler |
| `apexcharts` | – | **Dual license, not usable** | The community license is only for organizations with revenue under 2 million dollars, and it prohibits embedding in a product used by others (that requires a paid OEM license). Tabler's demo pages use it; use d3, ECharts (Apache 2.0) or Chart.js (MIT) instead. |

### Size and pitfall of the migration

The most frequent Bulma classes in the UI: `button` (128), `column`/`columns` (126), `title` (84), `is-small`, `is-primary`, `is-danger`, `table`, `tag`, `notification`; the custom `boxed-section` structure appears in about 250 places; HashiCorp Design System components are used in 119 places.

Bulma and Bootstrap partly use the same class names (`table`, `modal`, `dropdown`, `navbar`, `card`, `tag`), and Bootstrap globally overrides base styles. The two cannot coexist on the same page.

### Method

1. **Ember upgrade** to a supported major version, before any new components are built.
2. **Own component layer** (`ui/app/components/ui/`): button, card, table, badge, status indicator, tabs, modal, dropdown, empty state, step indicator, timeline, page header, form fields, code editor. Templates use these, not raw CSS classes; the 119 HashiCorp components are also replaced with these.
3. **Behavior from Ember:** instead of Bootstrap's JavaScript, Ember components and modifiers; from Tabler only the CSS and the Popper-based positioning.
4. **Isolation:** at build time the old Bulma styles are put into a `.legacy` scope, while Tabler is global; pages not yet migrated run in this scope. The new frame is Tabler from day one.
5. **Page by page:** the pages of the new features are built with Tabler from the start, the existing ones together with their renewal; at the end the `.legacy` scope and Bulma are removed.

### Tabler patterns for the plan

| Planned element | Tabler pattern |
| --- | --- |
| Grouped menu, top bar with selectors | Vertical navigation layout |
| Dashboard | Card grid, stat cards, status dots |
| Lists | Table and datagrid patterns |
| Restore and release wizard | Step indicator (steps) |
| Event stream, multiregion deployment, revisions | Timeline |
| Empty states, error pages | Empty state, error page |
| Dark mode | `data-bs-theme` |

## Schedule

| Phase | Content | Backend prerequisite |
| --- | --- | --- |
| 1. Foundation | Ember upgrade; Tabler and component layer; new frame and navigation; design tokens; dashboard; event stream client; removal of Optimize; `ember-intl` | – |
| 2. Existing areas | Job page with tabs; jobs, clients, allocations, Variables, access migrated; unified lists and editor | – |
| 3. New areas | Backups, quotas, admission, packages, objects, scaling, services with health, networking, multiregion deployments | the corresponding backend phases |
| 4. Completion | Migrating the remaining pages; removing the `.legacy` scope and Bulma; replacing the HashiCorp branding elements | phases 1–3 |

## Acceptance criteria

- Every page is operable by keyboard, with visible focus, and meets the WCAG 2.1 AA contrast requirements in light and dark mode.
- The dashboard in "All regions" mode with three regions loads within 2 seconds and updates from the event stream within 1 second.
- The migrated pages contain no Bulma classes, and the templates contain no raw Tabler classes outside the component layer.
- The UI is complete in English and Hungarian.
- The runnable package contains no ApexCharts or other dependency under a non-open license (license check in CI).

## Risks

| Risk | Mitigation |
| --- | --- |
| The Ember upgrade drags on | Gradual upgrade per LTS version; the new pages already in the modern component model |
| Bulma–Bootstrap style conflict during the transition | `.legacy` scope, page-by-page migration, visual regression tests |
| A Tabler upgrade breaks the UI | Component layer; pinned version; releases older than two weeks |
| License problem with a chart or UI library | List of allowed licenses and automatic check in CI |
| Too many new pages at once | Schedule tied to the backend phases; the new pages are built together with the feature |

## Open questions

- [x] What should the fork's final product name and branding be ("Norca")? Decided 2026-10-24: the name is Norca; the visual identity stays the neutral Tabler base (no custom logo or palette for now).
- [x] ECharts or Chart.js for the new charts? Decided 2026-10-24: ECharts.
- [x] Should the old UI remain available in parallel for a transition period (for example `/ui/legacy`)? Decided 2026-10-24: yes, at `/ui/legacy` for one release.
- [x] Which languages are needed beyond English and Hungarian? Decided 2026-10-24: English only; no translation layer (`ember-intl` is dropped from phase 1).
