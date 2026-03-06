# dread.sh Marketing Plan

## Product One-Liner

**Webhooks to your terminal and desktop.** Get desktop notifications and a live terminal feed from Stripe, GitHub, Sentry, and anything else that sends webhooks. Share your setup with the whole team in one command.

## Key Angles (use the right one per platform)

| Angle | When to use |
|---|---|
| **Developer tool** | "Real-time webhook monitoring in your terminal" — HN, r/programming, Dev.to |
| **Self-hosted / simple** | "Single binary, SQLite, no Docker, no dependencies" — r/selfhosted, Lobste.rs |
| **Team collaboration** | "One command to share webhook notifications with your whole team" — r/devops, IndieHackers |
| **Replaces dashboards** | "Stop switching between Stripe, GitHub, Sentry dashboards" — r/webdev, r/nextjs |
| **Go ecosystem** | "Built with Go, Bubble Tea TUI, embedded SQLite" — r/golang, Golang Weekly |
| **Indie / build story** | "I built a CLI tool that sends desktop notifications for webhooks" — r/SideProject, IndieHackers |
| **Integration-specific** | "Monitor Stripe payments from your desktop" — r/stripe, r/shopify |

---

## Launch Platforms

### 1. Hacker News (Show HN)

**When:** Tuesday–Thursday, 8–10am ET

**Title:**
```
Show HN: dread.sh – Webhooks to your terminal and desktop
```

**Text:**
```
I built dread.sh because I was tired of checking five different dashboards
to see if a Stripe charge went through, a deploy finished, or Sentry caught
something.

dread gives you a single terminal feed and desktop notifications for every
webhook event. One install, no accounts:

  curl -sSL dread.sh/install | sh
  dread new "Stripe Prod"     # creates a channel, gives you a webhook URL
  # paste the URL into Stripe → notifications start

It auto-detects Stripe, GitHub, Vercel, Sentry, Shopify, Linear, and 8
other services — parses headers and gives you human-readable summaries
instead of raw JSON.

Team use: run `dread share` to get a workspace ID. Teammates run
`dread follow ws_xxx` and get every channel, including new ones you add
later.

Features:
- Terminal TUI with event history, filtering, and JSON payload viewer
- Desktop notifications (launchd/systemd, runs at login)
- Webhook forwarding to localhost (no ngrok needed)
- Event replay for debugging
- Web dashboard at dread.sh/dashboard (no install needed)

Single Go binary, SQLite, no Docker, no accounts, no API keys.

Site: https://dread.sh
```

**Tips:**
- Engage with every comment
- If it doesn't land, repost in a few days
- Don't self-upvote or ask friends to — HN detects this

---

### 2. Product Hunt

**Tagline:** Webhooks to your terminal and desktop

**Description:**
```
dread.sh gives developers a single terminal feed and desktop notifications
for every webhook — Stripe, GitHub, Sentry, Vercel, and anything else.

Install in one command. No accounts, no API keys. Create channels, paste
webhook URLs into your services, and events start flowing to your terminal
and desktop immediately.

Teams: share your workspace with one command. Teammates get every channel
automatically.

Features:
- Terminal TUI with live event feed and JSON payload viewer
- Desktop notifications that run in the background at login
- Web dashboard (no install needed)
- Webhook forwarding to localhost for development
- Event replay for debugging
- Auto-detects 14+ webhook sources with human-readable summaries
- Filtering, event history, team workspaces
```

**First Comment (post immediately after launch):**
```
Hey PH! I built dread because I was constantly switching between Stripe,
GitHub, and Sentry dashboards to check if things were working.

Now I get a desktop notification the moment something happens — a failed
charge, a merged PR, a deploy error — and I can inspect the full payload
in my terminal.

The team feature is my favourite part: I set up channels once, teammates
run one command, and everyone gets notifications. New channels sync
automatically.

Would love to hear what integrations you'd want to see next.
```

**Tips:**
- Launch at 12:01am PT (Product Hunt resets at midnight PT)
- Have 5–10 people lined up to leave genuine reviews in the first hour
- Reply to every comment

---

### 3. Lobste.rs

**Title:**
```
dread.sh – Single-binary webhook monitoring with terminal TUI and desktop notifications
```

**Tags:** `go`, `cli`, `devops`

**Tips:**
- Lobste.rs is invite-only — you need an existing member to invite you
- Keep it technical, no marketing speak
- The "single binary, SQLite, no Docker" angle resonates here

---

## Reddit Posts

### 4. r/selfhosted

**Title:** `I built a single-binary webhook monitor with desktop notifications — no Docker, no dependencies`

**Body:**
```
I built dread.sh to monitor webhooks from Stripe, GitHub, Sentry, and other
services without needing multiple browser tabs open.

Install:
  curl -sSL dread.sh/install | sh

What it does:
- Creates webhook channels — paste the URL into Stripe/GitHub/wherever
- Desktop notifications for every event (runs as a background service)
- Terminal TUI with live feed, event history, and JSON payload viewer
- Web dashboard at dread.sh/dashboard
- Webhook forwarding to localhost for development
- Event replay for debugging

Stack: Single Go binary, embedded SQLite with WAL mode. No Docker, no
database server, no external dependencies.

Team use: teammates run `dread follow <workspace-id>` and get all your
channels automatically.

Auto-detects 14+ services (Stripe, GitHub, Vercel, Sentry, Shopify,
Linear, Slack, Discord, Twilio, SendGrid, AWS SNS, Supabase, Paddle, Svix)
and gives human-readable event summaries.

Site: https://dread.sh
Docs: https://dread.sh/docs
```

---

### 5. r/webdev

**Title:** `Stop switching between Stripe, GitHub, and Sentry dashboards — I built a tool that sends all webhook events to your terminal and desktop`

**Body:**
```
I kept losing time switching between dashboards to check if a deploy
finished, a payment went through, or an error was caught. So I built
dread.sh.

It gives you a single terminal feed and desktop notifications for every
webhook event from every service.

Quick start:
  curl -sSL dread.sh/install | sh
  dread new "Stripe Prod"

Paste the webhook URL into Stripe, and you start getting desktop
notifications and a terminal feed immediately.

It auto-detects Stripe, GitHub, Vercel, Sentry, Shopify, and 9 other
services — parses the webhook headers and shows you human-readable
summaries like "charge.succeeded $120.00 Visa ending 4242" instead of
raw JSON.

Other features:
- Forward webhooks to localhost (no ngrok)
- Replay past events for debugging
- Filter by source, type, or content
- Web dashboard at dread.sh/dashboard (no install needed)
- Team workspaces — one command to share with teammates

https://dread.sh
```

---

### 6. r/devops

**Title:** `Built a CLI tool for real-time webhook monitoring — desktop notifications for deploys, errors, and payments`

**Body:**
```
dread.sh gives you a unified feed of webhook events from your entire stack.
Desktop notifications + terminal TUI + web dashboard.

The use case that made me build it: I wanted to know immediately when a
Vercel deploy finished, a Stripe charge failed, or Sentry caught an error —
without watching three different dashboards.

Install:
  curl -sSL dread.sh/install | sh

Create channels, paste webhook URLs into your services, done. It runs as a
launchd (macOS) or systemd (Linux) service so notifications come through
even when you're not in the terminal.

Team use: `dread share` gives you a workspace ID. Teammates `dread follow`
it and get every channel, including ones you add later.

Features:
- Auto-detects 14+ webhook sources with human-readable summaries
- Terminal TUI with event history and JSON payload viewer
- Webhook forwarding to localhost for local development
- Event replay
- Web dashboard (no install needed)
- Single Go binary, SQLite, no external dependencies

https://dread.sh
```

---

### 7. r/SideProject

**Title:** `I built dread.sh — a CLI tool that turns webhooks into desktop notifications`

**Body:**
```
I was tired of checking Stripe, GitHub, and Sentry dashboards to see if
things were working. So I built dread.sh — it takes webhooks from all these
services and sends them to your terminal and desktop as notifications.

One install:
  curl -sSL dread.sh/install | sh

Create a channel, get a webhook URL, paste it into Stripe/GitHub/wherever.
Events start flowing immediately.

What makes it different:
- Single binary, no accounts, no API keys
- Auto-detects 14+ services and shows human-readable summaries
- Runs as a background service — notifications at login
- Terminal TUI for browsing event history and inspecting payloads
- Team workspaces — share with teammates in one command
- Web dashboard for when you're not at your terminal
- Forward webhooks to localhost (replaces ngrok for webhook development)

Built with Go, Bubble Tea for the TUI, embedded SQLite.

Would love feedback. What services would you want to monitor?

https://dread.sh
```

---

### 8. r/golang

**Title:** `dread.sh — webhook monitoring CLI built with Go, Bubble Tea, and embedded SQLite`

**Body:**
```
I built dread.sh, a CLI tool that aggregates webhooks from services like
Stripe, GitHub, and Sentry into a terminal TUI with desktop notifications.

Tech stack:
- Go with net/http for the server and WebSocket hub
- Bubble Tea for the terminal UI
- modernc.org/sqlite (pure Go SQLite) with WAL mode
- osascript (macOS) / notify-send (Linux) for desktop notifications
- Single binary, cross-compiled for darwin/linux, amd64/arm64

Architecture:
- Server receives webhooks at /wh/{channel-id}
- Auto-detects source from headers (Stripe-Signature, X-GitHub-Event, etc.)
- Parses payloads into human-readable summaries
- Broadcasts to connected WebSocket clients
- Stores events in SQLite for history/replay

The TUI connects via WebSocket and renders a live event feed. You can
inspect full JSON payloads, filter events, replay them to a local URL for
debugging, and forward live events to localhost.

Team feature: workspaces are published to the server. Teammates subscribe
and get all channels. New channels sync automatically on reconnect.

Install:
  curl -sSL dread.sh/install | sh

Site: https://dread.sh
```

---

### 9. r/programming

**Title:** `dread.sh — a single-binary CLI for real-time webhook monitoring with desktop notifications`

**Body:**
```
Webhooks are how services communicate — Stripe tells you about payments,
GitHub about pushes, Sentry about errors. But monitoring them means
checking multiple dashboards or tailing logs.

dread.sh gives you one terminal feed and desktop notifications for all of
them.

  curl -sSL dread.sh/install | sh
  dread new "Stripe Prod"
  # paste the webhook URL into Stripe → done

It auto-detects the source from HTTP headers and parses payloads into
human-readable summaries. A Stripe charge becomes "charge.succeeded $120.00
Visa ending 4242" instead of a wall of JSON.

Features:
- Terminal TUI with live feed, event history, JSON viewer
- Desktop notifications (launchd/systemd background service)
- Webhook forwarding to localhost (no ngrok)
- Event replay for debugging
- Team workspaces — share channels with one command
- Web dashboard at /dashboard
- Single Go binary, embedded SQLite, no dependencies

14+ auto-detected sources: Stripe, GitHub, Vercel, Sentry, Shopify,
Linear, Slack, Discord, Twilio, SendGrid, AWS SNS, Supabase, Paddle, Svix.

https://dread.sh
```

---

### 10. r/stripe

**Title:** `I built a tool that sends desktop notifications for Stripe webhooks — see charges, refunds, and disputes instantly`

**Body:**
```
I built dread.sh to get instant desktop notifications for Stripe events
without keeping the dashboard open.

Setup takes 30 seconds:
  curl -sSL dread.sh/install | sh
  dread new "Stripe Prod"

Paste the webhook URL into Stripe's webhook settings. Now you get desktop
notifications like:

  charge.succeeded $120.00 on Visa ending 4242
  invoice.payment_failed for cus_NffrFeUfNV2Hib
  charge.dispute.created $250.00 — reason: fraudulent

It detects the Stripe-Signature header automatically and parses the payload
into readable summaries.

You also get a terminal TUI for browsing event history, inspecting full
JSON payloads, and replaying events to your local webhook handler for
debugging.

Runs as a background service so notifications come through even when you're
not in the terminal.

Also works with GitHub, Sentry, Vercel, Shopify, and any other service
that sends webhooks.

https://dread.sh
```

---

### 11. r/nextjs

**Title:** `Get desktop notifications when your Vercel deploys finish (or fail) — no more watching the dashboard`

**Body:**
```
I built dread.sh because I was tired of refreshing the Vercel dashboard to
see if my deploy finished.

Now I get a desktop notification the moment it happens:

  deployment.ready dread-sh-git-main-a1b2c3.vercel.app promoted to production
  deployment.error build failed — exit code 1

Setup:
  curl -sSL dread.sh/install | sh
  dread new "Vercel Deploys"

Paste the webhook URL into your Vercel project settings → Webhooks.

It also works with GitHub (PR merges, pushes), Sentry (errors), Stripe
(payments), and any other webhook source. All events show up in one
terminal feed.

For local development: `dread --forward http://localhost:3000/api/webhook`
sends every real webhook event to your local Next.js server. No ngrok
needed.

https://dread.sh
```

---

### 12. r/commandline

**Title:** `dread — a Bubble Tea TUI for real-time webhook monitoring with desktop notifications`

**Body:**
```
dread.sh is a terminal-native webhook monitor. It gives you a live feed of
events from Stripe, GitHub, Sentry, and any service that sends webhooks.

TUI features:
- Live event feed with timestamps and human-readable summaries
- j/k navigation, enter to inspect full JSON payload
- / to filter by source, type, or content
- r to replay an event to a local URL
- Runs in the terminal alongside your other tools

Also sends native desktop notifications via a background service
(launchd on macOS, systemd on Linux).

Built with Go and Bubble Tea. Single binary install:
  curl -sSL dread.sh/install | sh

https://dread.sh
```

---

### 13. r/shopify

**Title:** `Desktop notifications for Shopify orders, refunds, and inventory changes — no more refreshing the admin`

**Body:**
```
I built dread.sh to get instant desktop notifications for Shopify webhook
events.

  curl -sSL dread.sh/install | sh
  dread new "Shopify Store"

Paste the webhook URL into Shopify Settings → Notifications → Webhooks.

Now you get desktop notifications like:
  orders/create Order #1042 — 3 items, $89.00 USD
  refunds/create Refund on Order #1038 — $45.00
  inventory_levels/update SKU-2847 stock changed

It detects the X-Shopify-Topic header automatically and gives you readable
summaries instead of raw JSON.

Also has a terminal TUI for browsing order history, inspecting full
payloads, and a web dashboard at dread.sh/dashboard.

Works with any webhook source — Stripe, GitHub, Sentry, etc. all in one
feed.

https://dread.sh
```

---

## Dev.to / Hashnode Articles

### 14. Dev.to — Tutorial Article

**Title:** `How I Built a Real-Time Webhook Monitor with Go, WebSockets, and Bubble Tea`

**Tags:** `go`, `webdev`, `opensource`, `tutorial`

**Outline:**
```
1. The problem — monitoring webhooks across multiple services is painful
2. Architecture overview — Go server, WebSocket hub, SQLite, Bubble Tea TUI
3. Auto-detecting webhook sources from HTTP headers
4. Parsing Stripe/GitHub/Sentry payloads into human-readable summaries
5. Building the real-time TUI with Bubble Tea
6. Desktop notifications with osascript and notify-send
7. Team workspaces — how channel sharing works
8. The web dashboard — vanilla JS, no build step
9. Try it: curl -sSL dread.sh/install | sh
```

**Tips:**
- Include code snippets from the actual codebase
- Add a GIF of the TUI in action
- Cross-post to Hashnode for double the reach
- Dev.to audience likes technical depth — don't just describe features, show how they work

---

### 15. Dev.to — Problem-Focused Article

**Title:** `Stop Checking Five Dashboards — Get All Your Webhook Events in One Terminal`

**Tags:** `webdev`, `devops`, `productivity`, `tooling`

**Outline:**
```
1. The dashboard fatigue problem
2. What if webhooks came to you instead?
3. Demo: setting up Stripe + GitHub + Sentry in 2 minutes
4. The team workflow — share with one command
5. Local development — forwarding + replay
6. Web dashboard for when you're away from terminal
7. Getting started
```

---

## Directories & Listings

### 16. awesome-selfhosted (GitHub PR)

**Section:** Automation / Monitoring

**Entry:**
```markdown
- [dread.sh](https://dread.sh) - Real-time webhook monitoring with terminal
  TUI, desktop notifications, and web dashboard. Single binary, SQLite.
  `Go` `MIT`
```

---

### 17. awesome-go (GitHub PR)

**Section:** Command Line / Utilities

**Entry:**
```markdown
- [dread](https://dread.sh) - Webhook monitoring CLI with Bubble Tea TUI,
  desktop notifications, WebSocket streaming, and embedded SQLite.
```

---

### 18. AlternativeTo

**List as alternative to:** Hookdeck, RequestBin, Webhook.site, Svix

**Description:**
```
dread.sh is a CLI tool that monitors webhooks from Stripe, GitHub, Sentry,
and any other service, delivering them as desktop notifications and a live
terminal feed. Single binary install, no accounts needed. Includes a web
dashboard, team workspaces, event replay, and webhook forwarding to
localhost.
```

---

### 19. SaaSHub

**Category:** Developer Tools / Monitoring

**Description:** Same as AlternativeTo entry above.

---

### 20. Console.dev

**Submit at:** https://console.dev/submit

**One-liner:** Real-time webhook monitoring CLI with desktop notifications, terminal TUI, and web dashboard.

---

### 21. ToolHunt.net / Uneed.best

**Description:** Same as AlternativeTo entry. Submit to both.

---

## Newsletters to Pitch

### 22. Golang Weekly

**Email pitch:**
```
Subject: dread.sh — webhook monitoring CLI built with Go, Bubble Tea, and embedded SQLite

Hi,

I built dread.sh, a CLI tool that aggregates webhooks from Stripe, GitHub,
Sentry, and other services into a terminal TUI with desktop notifications.

Tech: Go, Bubble Tea, modernc.org/sqlite (pure Go SQLite), WebSockets.
Single binary, cross-compiled for macOS/Linux.

Install: curl -sSL dread.sh/install | sh
Site: https://dread.sh

Would love to be featured in an upcoming issue.

Cheers,
Nigel
```

---

### 23. TLDR Newsletter

**Submit at:** https://tldr.tech/submit

**Pitch:**
```
dread.sh — webhooks to your terminal and desktop. One CLI tool that
monitors Stripe, GitHub, Sentry, and any other webhook source. Desktop
notifications, terminal TUI, web dashboard. Single binary install, no
accounts. Team workspaces let you share your setup in one command.

https://dread.sh
```

---

### 24. Console.dev Newsletter

**Submit at:** https://console.dev/submit

**Pitch:**
```
dread.sh is a developer CLI that aggregates webhook events from multiple
services (Stripe, GitHub, Sentry, Vercel, Shopify, etc.) into a single
terminal feed with desktop notifications. Auto-detects 14+ webhook sources,
parses payloads into human-readable summaries. Includes team workspaces,
event replay, webhook forwarding to localhost, and a web dashboard. Single
Go binary, embedded SQLite, no accounts or API keys needed.
```

---

### 25. Changelog News

**Submit at:** https://changelog.com/submit

**Pitch:** Same as Console.dev pitch above.

---

## Social Media

### 26. Twitter/X

**Launch tweet:**
```
I built dread.sh — webhooks to your terminal and desktop.

One CLI tool for Stripe, GitHub, Sentry, and anything else that sends
webhooks. Desktop notifications + terminal TUI + web dashboard.

curl -sSL dread.sh/install | sh

No accounts, no API keys, no Docker. Just webhooks.

[attach demo GIF]
```

**Follow-up tweet ideas:**
- "Stripe charge fails at 2am. You get a desktop notification before the customer emails you."
- "dread watch runs in the background. You get notified about deploys, payments, and errors without opening a single dashboard."
- "One command to share your webhook setup with teammates: `dread share`"
- Thread: "I built a webhook monitor. Here's the architecture." (Go, WebSockets, SQLite, Bubble Tea)

---

### 27. LinkedIn

**Post:**
```
I built dread.sh — a developer tool that sends desktop notifications for
webhook events from Stripe, GitHub, Sentry, and other services.

The problem: developers check multiple dashboards throughout the day to
see if a deploy finished, a payment went through, or an error was caught.

The solution: one terminal feed and desktop notifications for all of it.
Install in one command, create channels, paste webhook URLs, done.

Teams can share their setup with one command — new channels sync
automatically.

Try it: https://dread.sh

[attach demo GIF]
```

---

### 28. Bluesky

Same content as Twitter. Growing developer audience, less noise.

---

### 29. Discord Servers

**Post in #show-your-work or #projects channels in:**
- Self-Hosted Discord
- Golang Discord
- Webdev / Reactiflux Discord
- Indie Hackers Discord
- Dev.to Discord

**Template:**
```
Built dread.sh — a CLI tool that turns webhooks into desktop notifications.

Stripe, GitHub, Sentry, Vercel, and anything else that sends webhooks —
all in one terminal feed with desktop notifications.

curl -sSL dread.sh/install | sh

Single Go binary. No Docker, no accounts. Team workspaces, event replay,
web dashboard.

https://dread.sh
```

---

## YouTube

### 30. Demo Video (2–3 minutes)

**Script outline:**
```
1. "Webhooks are how services talk to you. But monitoring them means
    checking five different dashboards." (5s)

2. "dread.sh puts them all in one place." (3s)

3. Install: curl -sSL dread.sh/install | sh (show terminal, 10s)

4. Create channel: dread new "Stripe Prod" (show webhook URL, 10s)

5. Paste URL into Stripe dashboard (screen recording, 15s)

6. Trigger a test event from Stripe (10s)

7. Show desktop notification pop up (5s)

8. Show TUI with the event (10s)

9. Press enter — show full JSON payload (10s)

10. "Works with GitHub too." Create another channel, show GitHub push
    notification (20s)

11. Show web dashboard (15s)

12. "Share with your team: dread share" (10s)

13. "Try it: dread.sh" (5s)
```

---

## IndieHackers.com

### 31. Product Page

**Tagline:** Webhooks to your terminal and desktop

**Description:** Same as Product Hunt description.

### 32. Launch Post

**Title:** `I built dread.sh — desktop notifications for all your webhooks`

**Body:**
```
Hey IH!

I built dread.sh because I was spending too much time checking dashboards.
Stripe, GitHub, Sentry, Vercel — each one has its own UI and I'd forget to
check them.

dread takes webhooks from all these services and sends them to your desktop
as notifications. You also get a terminal UI for browsing event history and
inspecting payloads.

Install: curl -sSL dread.sh/install | sh
Create a channel: dread new "Stripe Prod"
Paste the webhook URL into Stripe. Done.

Team feature: run `dread share`, teammates run `dread follow`, everyone
gets notifications.

Right now it's free. I'm thinking about charging for unlimited channels and
longer event retention once I see what people actually use.

Would love feedback — especially on what integrations matter most to you.

https://dread.sh
```

---

## Rollout Schedule

| Week | Actions |
|---|---|
| **Week 1** | Record demo GIF. Post to r/SideProject, r/selfhosted. Submit to IndieHackers. |
| **Week 2** | Post to r/webdev, r/golang, r/commandline. Publish Dev.to tutorial article. |
| **Week 3** | Launch on Hacker News (Show HN). Post to Twitter/X, LinkedIn, Bluesky. |
| **Week 4** | Launch on Product Hunt. Email Golang Weekly, TLDR, Console.dev. |
| **Week 5** | Post to r/devops, r/programming. Submit PRs to awesome-selfhosted, awesome-go. |
| **Week 6** | Post to r/stripe, r/nextjs, r/shopify. Submit to AlternativeTo, SaaSHub, ToolHunt. |
| **Week 7** | Publish second Dev.to article. Post in Discord servers. Upload YouTube demo. |
| **Week 8** | Repost to Lobste.rs. Submit to Changelog News. Review what's working, double down. |

---

## Assets Needed Before Starting

- [ ] **Demo GIF** (30s) — webhook fires → notification pops up → TUI shows event → click to expand JSON
- [ ] **og:image** meta tag — so link previews look good when shared
- [ ] **Screenshots** — TUI, web dashboard, desktop notification
- [ ] **YouTube demo** (2–3 min) — can wait until week 4, but GIF is needed for week 1
