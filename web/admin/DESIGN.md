# PayGateway Admin — Design System

## 0. Reference Log
- **Primary reference**: Linear (clean, minimal, professional SaaS)
- **Secondary**: Vercel (breathable layout, typography), Supabase (subtle gradients, warm tones)
- **Tertiary**: Stripe (data density, clarity)

## 1. Design Tokens

### Color Palette

#### Light Theme
```
--bg-base:       #f8f9fa    (page background, very light slate)
--bg-surface:   #ffffff    (cards, panels)
--bg-subtle:    #f1f3f5    (code blocks, stat backgrounds)
--bg-hover:     rgba(0,0,0,0.04)  (hover state backgrounds)

--border:       #e9ecef    (card borders, dividers)
--border-strong:#dee2e6    (active borders)

--text-primary: #1a1d21    (headings, primary text - not pure black)
--text-body:    #3f4249    (body text - softer than black)
--text-muted:   #697386    (labels, captions, secondary info)
--text-placeholder: #9ca3af (input placeholders)

--accent:       #5c6ac4    (indigo primary - Linear style, not generic blue)
--accent-hover: #4959b0
--accent-light: #f0f1fb
--accent-gradient: linear-gradient(135deg, #5c6ac4, #7c7fe4)

--success:      #00875a    (deep green)
--success-bg:   #e3fcef
--warning:      #de8000    (deep amber)
--warning-bg:   #fff4e5
--danger:       #de350b    (deep red)
--danger-bg:    #ffebe6
--info:         #0065ff    (clean blue)
--info-bg:      #e5f0ff
```

#### Dark Theme
```
--bg-base:       #0d0f14    (deep navy black)
--bg-surface:   #151820    (cards - slightly lighter than bg)
--bg-subtle:     #1e2432    (code blocks, stat backgrounds)
--bg-hover:     rgba(255,255,255,0.06)

--border:        #2a2e3b    (card borders)
--border-strong: #3d4250

--text-primary:  #e8eaed
--text-body:     #c4c9d4
--text-muted:    #7c8194
--text-placeholder: #525968

--accent:        #818cf8    (lighter indigo for dark bg)
--accent-hover:  #a5b4fc
--accent-light:  #1e2432
--accent-gradient: linear-gradient(135deg, #818cf8, #6366f1)
```

### Typography
- **Primary font**: `"Inter", -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif`
- **Mono font**: `"JetBrains Mono", "Fira Code", "Consolas", monospace`
- **Scale**: 11 / 12 / 13 / 14 / 15 / 16 / 18 / 20 / 24 / 30 / 36px
- **Weights**: 400 (regular), 500 (medium), 600 (semibold), 700 (bold)

### Spacing (4px grid)
- `4px` xs, `8px` sm, `12px` md, `16px` lg, `24px` xl, `32px` 2xl, `48px` 3xl

### Shadows
- Card: `0 1px 3px rgba(0,0,0,0.06), 0 0 0 1px rgba(0,0,0,0.04)`
- Hover: `0 4px 16px rgba(0,0,0,0.1), 0 0 0 1px rgba(0,0,0,0.04)`
- Elevated: `0 8px 32px rgba(0,0,0,0.12)`

## 2. Component Tokens

### Sidebar
- Width: 220px
- Background: `--bg-surface`
- Border: `1px solid var(--border)`
- Active item: `--accent-light` background + left accent border (3px)
- Nav item height: 36px
- Brand area: logo mark (32px) + name (14px semibold)

### Topbar
- Height: 52px
- Background: `rgba(var(--bg-surface), 0.85)` + backdrop blur
- Border: `1px solid var(--border)`

### Cards (pg-card)
- Border-radius: 10px
- Border: `1px solid var(--border)`
- Background: `--bg-surface`
- Padding header: 16px 20px
- Padding body: 20px 20px
- No shadow on default (borders provide enough definition)
- Subtle shadow on hover

### Tables
- Row height: 48px
- Header height: 40px
- Cell padding: 12px 16px
- Border: `1px solid var(--border)`
- Row hover: `--bg-hover`
- Striped: alternate `--bg-base` background
- Font: 13px for data, 12px for secondary info

### Buttons
- Border-radius: 6px
- Height: 34px (default), 30px (small)
- Transition: 150ms ease

### Form Inputs
- Border-radius: 6px
- Height: 36px
- Border: `1px solid var(--border)`
- Focus: accent color border + subtle ring

## 3. Motion
- Hover transitions: 120ms ease
- Page transitions: 180ms ease-out (fade + slight Y translate)
- Easing: `cubic-bezier(0.25, 0.1, 0.25, 1)`
- No decorative animation (Stripe/Linear style - motion serves function)

## 4. Element Plus Overrides
- Component border-radius: 6px (uniform with design system)
- Tag border-radius: 4px
- Message/Notification border-radius: 10px
- Dialog border-radius: 12px
- Dropdown border-radius: 8px