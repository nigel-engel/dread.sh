#!/bin/sh
set -e

REPO="nigel-engel/dread.sh"
BINARY="dread"
INSTALL_DIR="/usr/local/bin"

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)

case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *) echo "Unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

case "$OS" in
  darwin|linux) ;;
  *) echo "Unsupported OS: $OS" >&2; exit 1 ;;
esac

TARBALL="${BINARY}_${OS}_${ARCH}.tar.gz"
URL="https://github.com/${REPO}/releases/latest/download/${TARBALL}"

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

echo "Downloading dread for ${OS}/${ARCH}..."
curl -sL "$URL" -o "$TMPDIR/$TARBALL"
tar -xzf "$TMPDIR/$TARBALL" -C "$TMPDIR"

if [ -w "$INSTALL_DIR" ]; then
  mv "$TMPDIR/$BINARY" "$INSTALL_DIR/$BINARY"
else
  echo "Installing to $INSTALL_DIR (requires sudo)..."
  sudo mv "$TMPDIR/$BINARY" "$INSTALL_DIR/$BINARY"
fi

chmod +x "$INSTALL_DIR/$BINARY"
echo "Installed dread to $INSTALL_DIR/$BINARY"

# Set up background notifications
if [ "$OS" = "darwin" ]; then
  PLIST="$HOME/Library/LaunchAgents/dev.dread.watch.plist"
  mkdir -p "$HOME/Library/LaunchAgents"
  cat > "$PLIST" << 'PLISTEOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>dev.dread.watch</string>
	<key>ProgramArguments</key>
	<array>
		<string>/usr/local/bin/dread</string>
		<string>watch</string>
	</array>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>/tmp/dread-watch.log</string>
	<key>StandardErrorPath</key>
	<string>/tmp/dread-watch.log</string>
	<key>ProcessType</key>
	<string>Background</string>
</dict>
</plist>
PLISTEOF
  launchctl bootout gui/$(id -u) "$PLIST" 2>/dev/null || true
  launchctl bootstrap gui/$(id -u) "$PLIST"
  echo "Background notifications enabled (launchd)"

elif [ "$OS" = "linux" ]; then
  UNIT_DIR="$HOME/.config/systemd/user"
  mkdir -p "$UNIT_DIR"
  cat > "$UNIT_DIR/dread-watch.service" << 'UNITEOF'
[Unit]
Description=dread webhook notifications
After=network-online.target

[Service]
ExecStart=/usr/local/bin/dread watch
Restart=always
RestartSec=3

[Install]
WantedBy=default.target
UNITEOF
  systemctl --user daemon-reload
  systemctl --user enable --now dread-watch.service
  echo "Background notifications enabled (systemd)"
fi

echo ""
echo "Next: dread new \"My Channel\""
