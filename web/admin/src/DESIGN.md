# PayGateway Admin — Design System Specification

> Inspired by Linear App / Vercel Dashboard — Ultra-minimal, precise, premium SaaS
> Reference: `linear.app.md` (Layer B) + `minimalist-skill.md` (Layer A)
> Status: v2.0 — Full redesign for Stripe/Linear/Vercel quality bar

## 0. Research Log

- **Layer A selected**: `minimalist-skill.md` — "minimal", "clean", "Linear-like" → premium utilitarian minimalism
- **Layer B selected**: `linear.app.md` — Linear's design tokens: near-black backgrounds, indigo-violet accent `#7170ff`, semi-transparent borders, Inter 510 signature weight, aggressive negative letter-spacing
- **Reference shortlist**: Linear (`linear.app.md`), Vercel (`vercel.md`), Supabase (`supabase.md`) — chosen: Linear for tokens + Vercel for login page inspiration
- **Design direction locked**: Linear-style dark-surface UI with indigo-violet accent. Light mode: off-white `#FAFAFA` bg, white cards, near-black text. Dark mode: near-black `#0D0D0F` bg, charcoal surfaces. Single accent `#7170ff` used sparingly. No gradients on large areas, no glassmorphism, no emoji icons.
- **Why Linear**: Unmatched precision — near-black canvas, semi-transparent borders (not dark borders on dark surfaces), 510 weight signature, aggressive tracking at display sizes. This is the reference that separates "Stripe-inspired" generic dark UI from real Linear-grade craft.

---

## 1. Visual Direction

**Atmosphere**: "Precision instrument" — darkness as native medium in dark mode, clean bone-white in light mode. Every element calibrated to the pixel. No decoration that doesn't serve function. The interface reads as engineered, not designed.

**Signature moments**:
- Sidebar: ultra-narrow (200px), near-invisible surface with whisper-thin borders
- Active nav item: accent-colored left border strip (2px), subtle bg tint — no gradient
- KPI cards: pure numbers, no background color blocks, no icons in colored circles
- Charts: single-accent-color line charts, muted gray secondary elements

**Banned elements** (per minimalist-skill):
- No `rounded-full` on cards, buttons, or primary elements
- No heavy drop shadows (none or ultra-subtle `rgba(0,0,0,0.04)`)
- No gradients on large backgrounds (accent allowed on small elements only)
- No emojis anywhere — SVG icons only
- No Inter/Roboto as generic defaults (Inter is fine IF used with 510 weight + cv01/ss03 features)

---

## 2. Color Palette

### Light Mode

| Token | Hex | Role |
|-------|-----|------|
| `--bg-base` | `#FAFAFA` | Page background — warm bone |
| `--bg-surface` | `#FFFFFF` | Cards, sidebar, topbar |
| `--bg-subtle` | `#F4F4F5` | Table header, subtle bg |
| `--bg-hover` | `rgba(0,0,0,0.04)` | Hover states |
| `--bg-active` | `rgba(0,0,0,0.06)` | Active/press states |
| `--border` | `#E4E4E7` | Primary borders |
| `--border-subtle` | `rgba(0,0,0,0.06)` | Whisper-thin dividers |
| `--text-primary` | `#09090B` | Headings, primary labels |
| `--text-body` | `#3F3F46` | Body copy |
| `--text-muted` | `#71717A` | Secondary/metadata |
| `--text-placeholder` | `#A1A1AA` | Placeholder text |
| `--text-inverse` | `#FFFFFF` | Text on dark/accent |
| `--accent` | `#7170FF` | Linear violet — CTAs, active states, links |
| `--accent-hover` | `#8284FF` | Hover on accent |
| `--accent-light` | `rgba(113,112,255,0.08)` | Accent tint bg |
| `--accent-muted` | `rgba(113,112,255,0.15)` | Slightly stronger accent bg |
| `--success` | `#22A069` | Status: success/active |
| `--success-bg` | `rgba(34,160,105,0.08)` | Success pill bg |
| `--warning` | `#C27803` | Status: pending/warning |
| `--warning-bg` | `rgba(194,120,3,0.08)` | Warning pill bg |
| `--danger` | `#D92D20` | Status: error/danger |
| `--danger-bg` | `rgba(217,45,32,0.08)` | Danger pill bg |
| `--info` | `#0A6FD4` | Status: info/neutral |
| `--info-bg` | `rgba(10,111,212,0.08)` | Info pill bg |

### Dark Mode

| Token | Hex | Role |
|-------|-----|------|
| `--bg-base` | `#0D0D0F` | Page background — Linear black |
| `--bg-surface` | `#18181B` | Cards, panels (Level 3) |
| `--bg-subtle` | `#222226` | Subtle surface tint |
| `--bg-sidebar` | `#111114` | Sidebar — darkest panel |
| `--bg-hover` | `rgba(255,255,255,0.05)` | Hover states |
| `--bg-active` | `rgba(255,255,255,0.07)` | Active/press |
| `--border` | `rgba(255,255,255,0.08)` | Primary border — semi-transparent white |
| `--border-subtle` | `rgba(255,255,255,0.05)` | Whisper-thin |
| `--text-primary` | `#FAFAFA` | Primary text — near-white |
| `--text-body` | `#C4C4C8` | Body text — silver |
| `--text-muted` | `#71717A` | Muted / metadata |
| `--text-placeholder` | `#52525B` | Placeholder |
| `--text-inverse` | `#09090B` | Text on light |
| `--accent` | `#7170FF` | Violet — consistent across modes |
| `--accent-hover` | `#8284FF` | Hover accent |
| `--accent-light` | `rgba(113,112,255,0.12)` | Accent tint |
| `--accent-muted` | `rgba(113,112,255,0.20)` | Stronger accent tint |
| `--success` | `#22A069` | Success |
| `--success-bg` | `rgba(34,160,105,0.12)` | Success bg |
| `--warning` | `#C27803` | Warning |
| `--warning-bg` | `rgba(194,120,3,0.12)` | Warning bg |
| `--danger` | `#F85149` | Error/danger |
| `--danger-bg` | `rgba(248,81,73,0.12)` | Danger bg |
| `--info` | `#79B8FF` | Info |
| `--info-bg` | `rgba(121,184,255,0.10)` | Info bg |

---

## 3. Typography

- **Primary font**: `'Inter', -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif`
- **Monospace**: `'JetBrains Mono', 'SF Mono', 'Consolas', monospace`
- **OpenType** (Linear signature): `font-feature-settings: 'cv01', 'ss03'` where feasible
- **Weights**: 400 (body/read), 510 (emphasis — Linear signature), 590 (strong emphasis — use sparingly)
- **Sizes**:
  - Page title: 14px / 590 / -0.01em
  - Card title: 14px / 590 / normal
  - Body: 13px / 400 / normal
  - Small/meta: 12px / 400 / normal
  - Caption: 11px / 510 / normal
  - Numbers/amounts: JetBrains Mono, tabular-nums
- **Never**: pure `#000000` or pure `#FFFFFF` for body text — always off-black/silver

---

## 4. Spacing System

Base: 8px grid
- Micro: 4px
- xs: 8px
- sm: 12px
- md: 16px
- lg: 20px
- xl: 24px
- 2xl: 32px
- Content gap (card grid): 16px
- Section gap: 16px
- Sidebar width: 200px (narrow, quiet)
- Topbar height: 48px
- Border radius: 8px (cards), 6px (inputs/buttons), 4px (tags/badges)

---

## 5. Component Primitives

### .pg-card
- Background: `var(--bg-surface)`
- Border: `1px solid var(--border)` — never shadow alone
- Border-radius: 8px
- No shadow on default; subtle shadow on hover in dark mode only: `0 2px 8px rgba(0,0,0,0.08)`
- Padding: 0 (header controls padding) / 20px body
- Hover: border darkens to `var(--border-subtle)` or accent

### Buttons
- **Primary**: `var(--accent)` bg, white text, 6px radius, no shadow
- **Ghost**: transparent bg, `1px solid var(--border)`, `var(--text-body)` text, 6px radius
- **Text/Link**: no border, no bg, accent text
- **All**: height 32px (default), 28px (small), 24px (mini) — Inter 510
- Hover: subtle bg tint — never gradient

### Inputs
- Background: `var(--bg-subtle)` or transparent with border
- Border: `1px solid var(--border)` — focus: accent border with subtle glow
- Border-radius: 6px
- No heavy box-shadows

### Tags/Badges
- Background: semantic color bg at 10% opacity
- Text: semantic color at full
- Border-radius: 4px
- Font: 11px / 510 / uppercase + letter-spacing 0.04em
- Padding: 2px 6px

### Tables
- Header: `--bg-subtle`, 11px / 510 / uppercase + tracking, muted text
- Rows: no stripe, hover bg `var(--bg-hover)`
- Border: `none` — border-collapse on wrapper only
- Cell padding: 10px 12px

### Sidebar
- Background: `var(--bg-surface)` (light) / `var(--bg-sidebar)` (dark)
- Border: `1px solid var(--border)` right side only
- Width: 200px
- Brand area: logo + name at top, no gradient background on logo
- Nav items: 34px height, 6px radius, left 2px accent border on active
- Active item: `--bg-hover` bg, `--accent` text

### Topbar
- Background: `var(--bg-surface)`
- Border: `1px solid var(--border)` bottom
- Height: 48px
- Left: page title (14px / 590)
- Right: icon buttons (32px), theme toggle

### Dialog/Modal
- Background: `var(--bg-surface)`
- Border: `1px solid var(--border)` or `rgba(255,255,255,0.08)` in dark
- Border-radius: 10px
- No heavy shadow — use border instead
- Backdrop: `rgba(0,0,0,0.4)` in dark / `rgba(0,0,0,0.15)` in light

### Charts (ECharts)
- Background: transparent
- Axis lines: `var(--border)` in light / `rgba(255,255,255,0.06)` in dark
- Primary series: `--accent` (`#7170FF`)
- Secondary series: `--text-muted`
- No gradient fills on large areas — use flat color or subtle opacity
- Grid: minimal, border-only

---

## 6. Motion

- **Duration**: 120-150ms for micro-interactions, 200ms for layout transitions
- **Easing**: `cubic-bezier(0.16, 1, 0.3, 1)` (Linear's signature — quick start, gentle settle)
- **Page transitions**: opacity + translateY(4px) fade-in
- **Hover states**: background color shift only — no scale, no transform (except buttons active state: subtle scale(0.98))
- **No ambient motion**: no floating orbs, no pulsing glows
- **Slop check**: if animation serves no informational purpose, remove it

---

## 7. Accepted Debt

- Inter is loaded without `cv01`/`ss03` OpenType features (CDN limitation) — accepted, Inter is still a clean choice
- JetBrains Mono loaded from Google Fonts (fallback to system mono is acceptable)
- No CSS container queries (broader compatibility over advanced layout)
- Element Plus component overrides use `:deep()` scoped styles — necessary for third-party library integration

---

## 8. File Change Map

| File | Change |
|------|--------|
| `theme.css` | Full rewrite — tokens, light/dark vars, Element Plus global overrides |
| `App.vue` | Refactor layout — narrower sidebar (200px), cleaner nav items, thinner topbar (48px), remove gradient logo bg |
| `Login.vue` | Strip all glassmorphism/orbs, flat white+accent or dark minimal card |
| `Dashboard.vue` | Strip KPI colored icon backgrounds, flat number display, clean chart palette |
| `Channels.vue` | Flat channel logos, clean stat grid |
| `Orders.vue` | Cleaner table, more minimal filter bar |
| Other views | Consistent pg-card styling, remove gradient backgrounds |