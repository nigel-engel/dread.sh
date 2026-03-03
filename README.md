# dread.sh

Webhooks to your terminal and desktop. Get desktop notifications and a live terminal feed from Stripe, GitHub, Sentry, and anything else that sends webhooks.

## Install

```sh
curl -sSL dread.sh/install | sh
```

Or with Homebrew:

```sh
brew install nigel-engel/tap/dread
```

## Quick start

```sh
# Create a channel to receive webhooks
dread new stripe-events

# Open the live terminal feed
dread

# Run in the background for desktop notifications
dread watch
```

Point your webhook provider at the URL shown by `dread new` and events stream to your terminal in real time.

## Features

- **Live terminal feed** — real-time TUI powered by Bubble Tea
- **Desktop notifications** — native macOS and Linux notifications via `dread watch`
- **Team workspaces** — share webhook feeds with your team in one command
- **Multiple sources** — Stripe, GitHub, Sentry, and any custom webhook
- **Self-hostable** — single Go binary, deploy anywhere

## CLI

| Command | Description |
|---|---|
| `dread` | Open the interactive TUI |
| `dread new <name>` | Create a new webhook channel |
| `dread list` | List all channels |
| `dread logs [channel]` | View event history |
| `dread status` | Show connection status |
| `dread test <channel>` | Send a test event |
| `dread watch` | Background mode with desktop notifications |
| `dread add <workspace>` | Follow a shared workspace |
| `dread remove <workspace>` | Unfollow a workspace |
| `dread replay <channel>` | Replay recent events |

## Self-hosting

```sh
# Build from source
make build

# Run the server
./bin/dread-server
```

The server uses SQLite for storage and runs on port 8080 by default. See the [documentation](https://dread.sh/docs) for full configuration options.

## Links

- [Documentation](https://dread.sh/docs)
- [Changelog](https://dread.sh/changelog)
