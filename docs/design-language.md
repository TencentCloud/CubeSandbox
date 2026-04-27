# CubeSandbox Design Language

## *Cube Console* — The Design System for a Next-Generation Agent Sandbox Cockpit

> **Positioning**: An "ultra-premium, minimal, yet electrifying" design language for CubeSandbox, calibrated against the console aesthetics of Vercel, Linear, Fly.io, and E2B, while serving CubeSandbox's core audience — **AI agent developers, platform operators, and SREs**.

---

## 1. UI/UX Overview

### 1.1 Design Philosophy (Three Iron Rules)

| Principle | Meaning | How It Shows Up |
|---|---|---|
| **Millisecond-Native** | Millisecond latency is the soul of the product; the UI must make it *felt*. | Every number animates in a monospace font (60ms, 5MB, P99 137ms); the sandbox-creation button embeds a live stopwatch; every load completes in under 200ms. |
| **Glass & Grid** | Minimalism + glassmorphism + data grids. | Deep black / midnight-blue base with frosted-glass layers; subtle CRT scanline accents; data aligned precisely to a 12- or 24-column grid. |
| **Operator-First** | This is an engineer's tool, not a slide deck. | Everything keyboard-driveable (⌘K command palette); high information density; dark by default; zero gratuitous motion. |

### 1.2 Design Tone

The visual system is anchored by three moods working in concert:

- **Cube Midnight** — A near-black, blue-tinted canvas that lets data glow rather than shout.
- **Cube Blue Spectrum** — A blue-violet accent range that echoes the logo's cube facets, used sparingly to signal primary action and live state.
- **Engineered Typography** — A tri-family stack pairing a geometric display face, a neutral UI face, and a precision monospace for every number, ID, and log line.

Motion is treated as a scarce resource: only three microinteractions ship by default — breathing pulses on live state, number-morph transitions on streamed metrics, and gradient shimmers on primary CTAs. All motion respects `prefers-reduced-motion`.

---

## 2. Color Palette (Design Tokens)

### 2.1 Surface — *Cube Midnight*

Layered neutrals that establish depth without glare. Lower numbers sit deeper in the z-axis.

```yaml
bg-0:     #070B10    # Deepest background (page)
bg-1:     #0C1117    # Card background
bg-2:     #151B24    # Hover / floating layer
border:   #1F2732    # 1px divider
text-1:   #E6EDF3    # Primary text
text-2:   #8B98A8    # Secondary text
text-3:   #4A5668    # Tertiary / hint text
```

### 2.2 Accent — *Cube Blue Spectrum*

A blue-to-violet arc that references the cube-facet logo, extended with semantic status hues.

```yaml
primary:        #5B8DEF    # Primary blue (CTAs, focus)
primary-glow:   #7AA7FF    # Glow / halo shade
accent-cyan:    #22D3EE    # Running / live
accent-violet:  #A78BFA    # Templates / snapshots
accent-amber:   #F59E0B    # Paused
accent-rose:    #F43F5E    # Danger / destroy
accent-emerald: #10B981    # Healthy
```

### 2.3 Gradients & Glass

```yaml
hero-gradient: linear-gradient(135deg, #5B8DEF 0%, #A78BFA 50%, #22D3EE 100%)
glass:         rgba(255, 255, 255, 0.04) + backdrop-filter: blur(16px)
```

- **Hero gradient** is reserved for top-level brand moments and primary CTAs — never as large background washes.
- **Glass** is used for floating panels, modals, and the command palette, placed over `bg-0` or `bg-1`.

### 2.4 Semantic Status Mapping

Color is never the sole carrier of meaning; every state also ships with a glyph and, where relevant, a motion cue.

| State | Color Token | Glyph | Motion |
|---|---|---|---|
| Running | `accent-cyan` | ⬤ | Breathing pulse, 1.6s |
| Paused | `accent-amber` | ❚❚ | Static |
| Creating | `primary` | ◎ | Rotating stroke |
| Error | `accent-rose` | ✕ | Single subtle shake |
| Healthy (node) | `accent-emerald` | ● | — |
| Unhealthy | `accent-rose` | ● | Flicker |

### 2.5 Accessibility Rules for Color

- All interactive components meet **WCAG AA** contrast (≥ 4.5:1) against their surface.
- Focus state uses a **2px `primary-glow` outer ring**, never relying on color alone.
- Status is always doubled up with a glyph so color-blind operators lose no information.
- Charts provide both visual encoding and an equivalent accessible table via `aria-label`.

---

## 3. Typography (Reference)

Included here because type selection is inseparable from the color system — numbers in monospace are how *Millisecond-Native* becomes visible.

```yaml
display: "GT Walsheim" / "Inter Display"   # Headlines
body:    "Inter"                           # UI / prose
mono:    "JetBrains Mono" / "Geist Mono"   # Numbers, IDs, code, logs
```

---

## 4. Radius & Motion (Reference)

```yaml
# Radius
r-sm: 6px   r-md: 10px   r-lg: 14px   r-xl: 20px

# Motion — less is more
ease-cube:     cubic-bezier(0.22, 1, 0.36, 1)
duration-fast: 120ms
duration-std:  220ms
duration-slow: 360ms
```

