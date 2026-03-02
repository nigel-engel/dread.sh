# Terminal Command Center — Universal Webhook Feed in Your Terminal

## The Problem

Developers check 10+ apps daily for updates — Stripe, GitHub, Sentry, Vercel, databases. Everything ends up in Slack or email, buried in noise. There's no clean, lightweight, real-time feed of everything happening across your stack.

## The Product

One terminal. Every app you use. Live.

```
┌──────────────────────────────────────────────┐
│ ⚡ live feed                                  │
│                                              │
│ 14:01  stripe    Payment $120.00 received     │
│ 14:03  github    PR #42 merged by @dan        │
│ 14:05  supabase  Booking #1042 created        │
│ 14:06  vercel    Deploy succeeded — prod       │
│ 14:08  sentry    TypeError in /api/bookings    │
│ 14:12  stripe    Subscription cancelled        │
│ 14:14  github    Issue #88 opened              │
│ █                                              │
└──────────────────────────────────────────────┘
```

## How It Works

```
Stripe webhooks  ──┐
GitHub webhooks  ──┤
Supabase realtime ─┤──▶  Server        ──▶  Terminal client
Sentry webhooks  ──┤     (routes events)     (displays live)
Vercel webhooks  ──┤
Any webhook      ──┘
```

Every SaaS app already supports webhooks. The server receives them, normalizes the data, and streams it to connected terminal clients.

## Config

```yaml
sources:
  stripe:
    secret: whsec_...
    format: "{{event}} — {{amount}}"
  github:
    secret: gh_...
    format: "{{action}} on {{repo}}"
  sentry:
    secret: sentry_...
    format: "{{error}} in {{url}}"
  supabase:
    url: https://xxx.supabase.co
    key: sb_...
    tables: [bookings, payments, customers]
    format: "{{table}} — {{type}} — #{{id}}"
```

## Why This Over Existing Tools

| Tool | Problem |
|---|---|
| Slack integrations | Noisy, buried in channels, browser-based |
| Datadog/Grafana | Heavy, expensive, overkill for small teams |
| wtfutil | Static polling, not real-time |
| PagerDuty | Alert routing only, $21/user/mo |
| Email notifications | Slow, lost in inbox |

This is: lightweight, real-time, self-hostable, terminal-native.

## Target Users

- Indie developers monitoring their SaaS
- Small DevOps/SRE teams
- Freelancers managing multiple client projects
- Anyone who wants a live dashboard without leaving the terminal

## Business Model

| Tier | What | Price |
|---|---|---|
| Open source | Self-host server + CLI | Free |
| Hosted | Managed server, just connect your webhooks | $9-15/mo |
| Team | Shared feeds, routing, on-call alerts | $20-30/seat/mo |

### What people pay for
- Not having to self-host (just get a webhook URL)
- Reliability — guaranteed uptime, retries, delivery
- Team features — shared feeds, routing rules, on-call rotation
- Pre-built integrations for 50+ services
- Searchable event history
- Escalation — SMS/call if nobody acknowledges an alert

### Revenue projection
- 1,000 free users → 30-50 paying → $400-600/mo
- 10,000 free users → 300-500 paying → $4-6k/mo
- Team tier scales higher with seat-based pricing

## Competitive Advantage

- Open source core builds trust and adoption
- Terminal-native = zero overhead, runs anywhere
- Self-hostable = privacy-conscious users onboard without hesitation
- Webhook-based = works with any app that has an API, no custom integrations needed
- Can evolve into team chat (humans + systems in one feed)

## Tech Decisions to Make

- **Language**: Go (single binary, good concurrency) vs Rust vs TypeScript
- **TUI framework**: Bubble Tea (Go), ratatui (Rust), Ink (TypeScript)
- **Transport**: WebSocket vs SSE vs SSH
- **Storage**: Ephemeral only vs optional SQLite for history
- **Hosting**: Fly.io, Railway, or raw VPS for the managed tier

## Build Order

### Phase 1 — MVP
- Server that receives webhooks from 2-3 sources (Stripe, GitHub, generic)
- Terminal client that connects and displays live feed
- Basic config file for sources and formatting

### Phase 2 — Integrations
- Pre-built parsers for top 10 webhook providers
- Supabase realtime support
- Filtering and muting by source/event type

### Phase 3 — Hosted Tier
- Multi-tenant hosted server
- Auth and API keys
- Billing (Stripe)
- Dashboard for managing webhook URLs

### Phase 4 — Teams
- Shared feeds
- Routing rules (send payments to @alice, errors to @bob)
- On-call rotation
- Escalation (terminal → SMS → phone call)

### Phase 5 — Chat Convergence
- Humans can send messages into the same feed
- React to events with comments
- Terminal command center = systems + people in one place
