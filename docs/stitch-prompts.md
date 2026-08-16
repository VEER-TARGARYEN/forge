# FORGE GUI — Google Stitch prompt kit

Paste **Prompt 0** first. Stitch carries style across a session, so everything
after it inherits the system. Then run each screen prompt in order, one at a
time.

A note on getting something genuinely new out of Stitch: adjectives like
"minimal" and "Gen Z" produce averages, because every model has seen ten
thousand of them. What produces something distinctive is **naming the specific
rule you are breaking**. Every prompt below states a prohibition ("no cards, no
borders") next to a signature move ("a 10px uppercase micro-label labels every
region instead of a heading"). Keep that structure when you edit these.

---

## Prompt 0 — the design system

> Design a desktop web app called **FORGE**, an AI coding agent that edits code
> on your machine. The aesthetic is **"terminal luxury"** — the restraint of a
> terminal with the typographic care of a fashion editorial. Hyper-minimal, near
> zero chrome, built almost entirely from type and negative space.
>
> **Colour — dark by default:**
> - Canvas `#0A0A0B` (near-black, very slightly cool)
> - Raised surface `#111113`, used as a whisper-tint only, never with a border
> - Text primary `#F2F2F0` (warm white), secondary `#7A7A78`, tertiary `#4A4A48`
> - Single accent `#D8FF3E` (acid lime), used at most **twice per screen**
> - Positive `#6EE7A8`, negative `#FF5C5C`, warning `#FFB84D`
> - No gradients. The only divider is a 1px hairline at `rgba(255,255,255,0.06)`
>
> **Type — one family, a geometric monospace, nothing else:**
> - Display 44px / weight 500 / tracking -0.03em
> - Body 14px / weight 400 / line-height 1.6
> - **Micro-label 10px / weight 500 / UPPERCASE / tracking 0.18em / tertiary**
> - The micro-label is the signature element: it labels every region of the
>   interface *instead of* a conventional heading.
>
> **Hard rules:**
> - **No cards. No panels. No borders. No drop shadows.** Nothing that looks
>   like a container.
> - Hierarchy comes from exactly three things: type scale, colour value, and
>   spacing on an 8px grid.
> - Margins are generous — 64px or more on desktop.
> - Pills (fully rounded) are the **only** rounded shapes, and only ever on
>   interactive elements. Everything else has square corners.
> - A 1px vertical hairline runs down the left edge of any list of events, like
>   a timeline spine.
> - A film-grain noise texture sits over the whole canvas at 3% opacity.
>
> Also provide a light theme: canvas `#FAFAF8`, text `#0A0A0B`, accent
> `#5B21B6` deep violet. Same rules.

---

## Prompt 1 — Session (the main screen)

> The primary screen: a live agent session, full-bleed, no sidebar and no
> top navigation bar.
>
> Top-left, a micro-label reads `FORGE / SESSION`. Top-right, three micro-labels
> in a row showing live state: `GEMINI 3.5 FLASH`, `4.2K / 128K`, `00:47`. The
> token figure is the only accent-coloured element up there, and it shifts to
> warning colour as it fills.
>
> The centre is a **vertical timeline**, not a chat. A 1px hairline runs down
> the left. Each event hangs off that rail:
> - the agent's prose in body text, plain, no bubble
> - a tool call as a single dense line: a small filled square in the accent
>   colour on the rail, then `read_file` in primary text, then
>   `internal/router/router.go` in secondary, then `84 lines` in tertiary,
>   right-aligned
> - a completed step collapses to one line; the current step is expanded
>
> Pinned to the very bottom edge: a **borderless input**. No box around it — just
> a blinking accent caret and placeholder text `what should i build?` in
> secondary. Above the input, a 1px full-width progress hairline that fills with
> accent colour as the agent works. That hairline is the only progress
> indicator anywhere in the app — no spinners, no percentages.
>
> Show the session mid-run: four completed steps collapsed, one expanded and
> streaming.

---

## Prompt 2 — Diff approval (the interruptive moment)

> The screen where the agent asks permission to edit a file. This must feel
> like the interface holding its breath.
>
> The entire canvas dims to 40% except the diff. No modal, no dialog box, no
> border — the dimming *is* the modal.
>
> Micro-label at top: `APPROVE / 1 OF 1`. Below it, the file path
> `cmd/forge/embed.go` at 24px, and `+4 −1` where `+4` is positive-coloured and
> `−1` negative.
>
> The diff is **full-bleed with no gutter**. Added lines get a positive-colour
> background tint at 8% opacity across the full row; removed lines a negative
> tint. No `+`/`−` symbols at all — the tint carries the meaning. Line numbers
> sit in tertiary at 10px.
>
> At the bottom, three pill buttons in a row, generously spaced:
> `Y APPROVE` (accent fill, black text), `N SKIP` (outline), `Q ABORT`
> (outline, negative text). The leading letter of each is the keyboard shortcut,
> shown in the accent colour.
>
> Nothing else on screen.

---

## Prompt 3 — Command palette

> A single centred overlay, invoked by ⌘K, that is the **only** navigation in
> the entire app — there is no menu or sidebar anywhere.
>
> It floats over a blurred, dimmed canvas. No border, no card — just text on a
> `#111113` tint with 48px of internal padding.
>
> A borderless search input at the top with an accent caret. Below, results
> grouped under micro-labels: `SESSIONS`, `PROJECTS`, `ACTIONS`, `PROVIDERS`.
> Each row is one line: an action name in primary, its context in secondary
> right-aligned. The selected row is marked by a 2px accent bar on its left
> edge — **no background highlight**.
>
> At the very bottom, a row of tertiary 10px hints: `↑↓ navigate`,
> `↵ select`, `esc close`.

---

## Prompt 4 — Providers & health

> A status screen showing which AI providers are alive. Data-dense but airy.
>
> Micro-label `PROVIDERS`. Below, a list where each row is one provider,
> separated only by 1px hairlines — no table, no columns headers, no zebra
> striping.
>
> Each row: a 6px filled circle (positive / warning / negative), the provider
> name at 14px, the model id in secondary, and on the right a small **sparkline**
> of recent latency drawn as a 1px accent line, then the latency in tertiary.
>
> A provider that is rate-limited shows its row at 40% opacity with a
> warning-coloured micro-label `COOLING 51S` where the sparkline was.
>
> Above the list, three enormous statistics in display type with micro-labels
> beneath them: `2.4M` / `TOKENS TODAY`, `98.2%` / `SUCCESS`, `340MS` / `MEDIAN`.
> These three numbers are the largest type anywhere in the product.

---

## Prompt 5 — Semantic search

> A code search screen. A single borderless input at the very top with a large
> accent caret, placeholder `where is rate limiting handled?`.
>
> Results are **not cards**. Each is: a micro-label showing
> `INTERNAL/ROUTER/HEALTH.GO · 130-149`, then three lines of syntax-highlighted
> code at 13px mono, then 32px of empty space before the next result. Syntax
> highlighting uses only three colours — primary, secondary, and accent for the
> matched term.
>
> On the right of each result, a tiny relevance bar: a 2px vertical accent line
> whose height encodes the score.
>
> Above the results, a single toggle rendered as two pills: `HYBRID` (accent
> filled) and `KEYWORD` (outline).

---

## Prompt 6 — Mobile session

> The same session screen at 390×844. FORGE must run on any device, so this is a
> first-class screen, not a shrunken desktop.
>
> The timeline rail moves to 20px from the left edge. Tool-call rows compress to
> two lines: name and path on the first, result on the second. All touch targets
> are at least 44px tall despite the small type.
>
> The input sits above the keyboard with a pill-shaped `SEND` button in accent.
> The live status (model, tokens, elapsed) collapses into a single scrolling
> micro-label ticker across the very top.
>
> Add a bottom sheet, dragged up, showing the current step's full detail — no
> handle bar, just a hairline and generous padding.

---

## Prompt 7 — First run

> The empty state, shown before any provider is connected. Almost nothing on
> screen.
>
> Centred: the word `FORGE` in display type, then one line of secondary body
> text: `a coding agent that runs on your machine`. Beneath that, 96px of empty
> space, then a single accent pill button `CONNECT A PROVIDER`.
>
> Bottom-left, three tertiary micro-labels stacked, showing what works offline
> already: `SEARCH · READY`, `REPO MAP · READY`, `AGENT · NEEDS A MODEL`.
>
> No illustration, no hero image, no gradient. The emptiness is the design.

---

## Iterating in Stitch

- Generate **one screen per prompt**. Asking for several at once averages them.
- If output looks generic, the fix is almost always to add a prohibition, not
  another adjective. "No cards" moves the needle; "sleek and modern" does not.
- Ask for a variant with: *"same screen, but the accent is used exactly once"* —
  it usually improves.
- Export to Figma, then to code. Keep the design tokens from Prompt 0 as CSS
  custom properties so the front end and the design stay in sync.

## Turning it into an installable app

The design above is deliberately implementable as a plain web app first, then
wrapped:

| target | how | notes |
|---|---|---|
| **Any device, zero install** | PWA — `manifest.json` + service worker | Installs from the browser on desktop, Android, iOS. Simplest path. |
| **Desktop app** | Tauri v2 wrapping the same web UI | ~5 MB binary vs Electron's ~120 MB, and Rust-side access to the local filesystem. |
| **The backend** | `forge serve` already exposes an OpenAI-compatible HTTP API on `127.0.0.1:4000` | The GUI can talk to the existing binary over HTTP; no rewrite needed. |

The cleanest architecture: the GUI is a static web build talking to `forge
serve` over localhost. One binary, one web bundle, installable anywhere.
