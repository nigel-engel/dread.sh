package desktop

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

type Channel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func Run(serverURL string, channels []Channel) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("desktop overlay is currently macOS only (requires macOS + swiftc)")
	}

	if len(channels) == 0 {
		return fmt.Errorf("no channels configured — run: dread new \"My Channel\"")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	configDir := filepath.Join(home, ".config", "dread")
	appDir := filepath.Join(configDir, "DreadDesktop.app")
	appBin := filepath.Join(appDir, "Contents", "MacOS", "DreadDesktop")
	htmlPath := filepath.Join(configDir, "desktop.html")

	// Generate HTML with config baked in
	if err := generateHTML(htmlPath, serverURL, channels); err != nil {
		return fmt.Errorf("generating overlay HTML: %w", err)
	}

	// Compile Swift app if not already built
	if _, err := os.Stat(appBin); os.IsNotExist(err) {
		if _, err := exec.LookPath("swiftc"); err != nil {
			return fmt.Errorf("swiftc not found — install Xcode Command Line Tools: xcode-select --install")
		}
		fmt.Print("Building DreadDesktop.app... ")
		if err := compileApp(appDir); err != nil {
			return fmt.Errorf("compiling: %w", err)
		}
		fmt.Println("done")
	}

	// Kill any existing instance
	exec.Command("pkill", "-f", "DreadDesktop.app").Run()

	// Launch
	cmd := exec.Command(appBin, htmlPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launching overlay: %w", err)
	}

	fmt.Println("Dread Desktop is running")
	fmt.Println("  Widget is pinned to your desktop (bottom-right)")
	fmt.Println("  Drag it anywhere you want")
	fmt.Println("  D in menu bar to show/hide")
	fmt.Println("  Cmd+Q to quit")
	return nil
}

func Rebuild() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("desktop overlay is currently macOS only")
	}

	home, _ := os.UserHomeDir()
	appDir := filepath.Join(home, ".config", "dread", "DreadDesktop.app")

	// Kill running instance
	exec.Command("pkill", "-f", "DreadDesktop.app").Run()

	// Remove old build
	os.RemoveAll(appDir)

	fmt.Print("Rebuilding DreadDesktop.app... ")
	if err := compileApp(appDir); err != nil {
		return fmt.Errorf("compiling: %w", err)
	}
	fmt.Println("done")
	return nil
}

func Stop() {
	exec.Command("pkill", "-f", "DreadDesktop.app").Run()
	fmt.Println("Dread Desktop stopped.")
}

func compileApp(appDir string) error {
	macosDir := filepath.Join(appDir, "Contents", "MacOS")
	if err := os.MkdirAll(macosDir, 0755); err != nil {
		return err
	}

	tmpFile, err := os.CreateTemp("", "dread-desktop-*.swift")
	if err != nil {
		return err
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(swiftSource); err != nil {
		tmpFile.Close()
		return err
	}
	tmpFile.Close()

	binPath := filepath.Join(macosDir, "DreadDesktop")
	out, err := exec.Command("swiftc",
		tmpFile.Name(),
		"-o", binPath,
		"-framework", "Cocoa",
		"-framework", "WebKit",
		"-suppress-warnings",
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s\n%s", err, out)
	}

	// Write Info.plist
	plistPath := filepath.Join(appDir, "Contents", "Info.plist")
	return os.WriteFile(plistPath, []byte(infoPlist), 0644)
}

func generateHTML(path, serverURL string, channels []Channel) error {
	// Convert HTTP URL to WebSocket URL
	wsURL := strings.Replace(serverURL, "https://", "wss://", 1)
	wsURL = strings.Replace(wsURL, "http://", "ws://", 1)

	channelsJSON, err := json.Marshal(channels)
	if err != nil {
		return err
	}

	html := strings.NewReplacer(
		"%%SERVER_WS%%", wsURL,
		"%%CHANNELS_JSON%%", string(channelsJSON),
	).Replace(htmlTemplate)

	return os.WriteFile(path, []byte(html), 0644)
}

const infoPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
<key>CFBundleIdentifier</key><string>dev.dread.desktop</string>
<key>CFBundleName</key><string>DreadDesktop</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>CFBundleVersion</key><string>1.0</string>
<key>LSUIElement</key><true/>
<key>NSAppTransportSecurity</key>
<dict><key>NSAllowsArbitraryLoads</key><true/></dict>
</dict>
</plist>
`

const swiftSource = `
import Cocoa
import WebKit

class Overlay: NSObject, NSApplicationDelegate, WKScriptMessageHandler {
    var statusItem: NSStatusItem!
    var panel: NSPanel!
    var webView: WKWebView!
    var badge: Int = 0

    func applicationDidFinishLaunching(_ n: Notification) {
        NSApp.setActivationPolicy(.accessory)

        // Menu bar icon
        statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        if let b = statusItem.button {
            b.title = " D "
            b.font = NSFont.systemFont(ofSize: 13, weight: .bold)
            b.action = #selector(toggle)
            b.target = self
        }

        // Floating panel — always on desktop
        let w: CGFloat = 360, h: CGFloat = 480
        panel = NSPanel(
            contentRect: NSRect(x: 0, y: 0, width: w, height: h),
            styleMask: [.nonactivatingPanel, .fullSizeContentView, .borderless, .resizable],
            backing: .buffered, defer: false
        )
        panel.isFloatingPanel = true
        panel.level = .floating
        panel.collectionBehavior = [.canJoinAllSpaces, .stationary]
        panel.backgroundColor = .clear
        panel.isOpaque = false
        panel.hasShadow = true
        panel.isMovableByWindowBackground = true
        panel.minSize = NSSize(width: 280, height: 300)
        panel.maxSize = NSSize(width: 500, height: 900)

        // Vibrancy blur
        let vfx = NSVisualEffectView(frame: NSRect(x: 0, y: 0, width: w, height: h))
        vfx.autoresizingMask = [.width, .height]
        vfx.material = .hudWindow
        vfx.blendingMode = .behindWindow
        vfx.state = .active
        vfx.wantsLayer = true
        vfx.layer?.cornerRadius = 12
        vfx.layer?.masksToBounds = true
        panel.contentView = vfx

        // WebView with JS bridge
        let cfg = WKWebViewConfiguration()
        cfg.preferences.setValue(true, forKey: "developerExtrasEnabled")
        cfg.userContentController.add(self, name: "dread")
        webView = WKWebView(frame: vfx.bounds, configuration: cfg)
        webView.autoresizingMask = [.width, .height]
        webView.setValue(false, forKey: "drawsBackground")
        vfx.addSubview(webView)

        // Load HTML
        let path = CommandLine.arguments.count > 1 ? CommandLine.arguments[1] : ""
        if !path.isEmpty, let html = try? String(contentsOfFile: path, encoding: .utf8) {
            webView.loadHTMLString(html, baseURL: nil)
        }

        // Keyboard shortcuts
        NSEvent.addLocalMonitorForEvents(matching: .keyDown) { [weak self] event in
            if event.keyCode == 53 {
                self?.panel.orderOut(nil)
                return nil
            }
            if event.modifierFlags.contains(.command) && event.characters == "q" {
                NSApp.terminate(nil)
                return nil
            }
            return event
        }

        // Position bottom-right and show immediately
        if let screen = NSScreen.main {
            let margin: CGFloat = 20
            let x = screen.visibleFrame.maxX - w - margin
            let y = screen.visibleFrame.minY + margin
            panel.setFrameOrigin(NSPoint(x: x, y: y))
        }
        panel.makeKeyAndOrderFront(nil)
    }

    // JS bridge — handles messages from the web view
    func userNotificationCenter(_ center: Any, willPresent notification: Any) {}
    func userContentController(_ controller: WKUserContentController, didReceive message: WKScriptMessage) {
        guard let body = message.body as? String else { return }
        if body == "minimize" {
            panel.orderOut(nil)
        }
    }

    @objc func toggle() {
        if panel.isVisible {
            panel.orderOut(nil)
        } else {
            panel.makeKeyAndOrderFront(nil)
        }
    }
}

let app = NSApplication.shared
let d = Overlay()
app.delegate = d
app.run()
`

const htmlTemplate = `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
* { margin: 0; padding: 0; box-sizing: border-box; }
:root {
  /* Zinc neutrals (dark mode) */
  --zinc-50:  oklch(98.5% 0.001 256);
  --zinc-100: oklch(96.7% 0.003 256);
  --zinc-200: oklch(92% 0.003 256);
  --zinc-300: oklch(87.1% 0.003 256);
  --zinc-400: oklch(70.5% 0.003 256);
  --zinc-500: oklch(55.2% 0.003 256);
  --zinc-600: oklch(40% 0.003 256);
  --zinc-700: oklch(32% 0.003 256);
  --zinc-800: oklch(23% 0.003 256);
  --zinc-900: oklch(16% 0.003 256);
  --zinc-950: oklch(10% 0.003 256);

  /* Semantic */
  --bg: transparent;
  --card: oklch(16% 0.003 256 / 0.55);
  --card-hover: oklch(23% 0.003 256 / 0.6);
  --border: oklch(23% 0.003 256 / 0.7);
  --text: var(--zinc-50);
  --text-muted: var(--zinc-400);
  --text-dim: var(--zinc-500);

  /* Palette — badge/border accents (dark mode = 500 for borders, 700 for badge bg) */
  --red-500:      oklch(63.7% 0.237 25.33);
  --red-700:      oklch(50.5% 0.213 27.52);
  --orange-500:   oklch(68% 0.195 45);
  --orange-700:   oklch(52% 0.16 45);
  --amber-500:    oklch(76.9% 0.188 70);
  --amber-700:    oklch(55.5% 0.163 70);
  --yellow-500:   oklch(79.5% 0.184 95);
  --yellow-700:   oklch(55.4% 0.14 95);
  --green-400:    oklch(79.2% 0.209 151.71);
  --green-500:    oklch(72.3% 0.219 149.58);
  --green-700:    oklch(52.7% 0.154 150.07);
  --emerald-500:  oklch(69.6% 0.17 162.48);
  --emerald-700:  oklch(50.8% 0.118 165.61);
  --teal-500:     oklch(68% 0.15 200);
  --teal-700:     oklch(50% 0.11 200);
  --sky-500:      oklch(68.5% 0.169 237.32);
  --sky-700:      oklch(50% 0.134 242.75);
  --sapphire-300: oklch(81% 0.11 256);
  --sapphire-500: oklch(62% 0.22 256);
  --sapphire-700: oklch(48% 0.22 256);
  --indigo-500:   oklch(58.5% 0.233 277.12);
  --indigo-700:   oklch(45.7% 0.24 277.02);
  --violet-500:   oklch(60.6% 0.25 292.72);
  --violet-700:   oklch(49.1% 0.27 292.58);
  --purple-500:   oklch(62.7% 0.265 303.9);
  --purple-700:   oklch(49.6% 0.265 301.92);
  --fuchsia-500:  oklch(66.7% 0.295 322.15);
  --fuchsia-700:  oklch(51.8% 0.253 323.95);
  --pink-500:     oklch(65.6% 0.241 354.31);
  --pink-700:     oklch(52.5% 0.223 3.96);
  --rose-500:     oklch(64.5% 0.246 16.44);
  --rose-700:     oklch(51.4% 0.222 16.94);

  /* JSON syntax (light shades for dark bg) */
  --json-key:    oklch(81% 0.11 256);
  --json-string: oklch(87.1% 0.15 154.45);
  --json-number: oklch(87.9% 0.169 70);
  --json-bool:   oklch(81.1% 0.111 293.57);
  --json-null:   oklch(83% 0.135 45);

  /* Radius */
  --radius-sm: 8px;
  --radius-card: 10px;
}
body {
  font-family: -apple-system, BlinkMacSystemFont, 'SF Pro Text', sans-serif;
  background: var(--bg);
  color: var(--text);
  font-size: 13px;
  line-height: 1.4;
  overflow: hidden;
  height: 100vh;
  display: flex;
  flex-direction: column;
  -webkit-user-select: none;
  user-select: none;
}
.header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 14px 14px 10px;
  border-bottom: 1px solid var(--border);
  -webkit-app-region: drag;
}
.header-left {
  display: flex;
  align-items: center;
  gap: 8px;
}
.logo {
  font-size: 15px;
  font-weight: 700;
  letter-spacing: -0.3px;
}
.channel-count {
  font-size: 11px;
  color: var(--text-dim);
  background: oklch(23% 0.003 256 / 0.5);
  padding: 2px 7px;
  border-radius: 10px;
}
.header-right {
  display: flex;
  align-items: center;
  gap: 10px;
  -webkit-app-region: no-drag;
}
.status {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 11px;
  font-weight: 500;
}
.minimize-btn {
  background: none;
  border: none;
  color: var(--text-dim);
  font-size: 16px;
  cursor: pointer;
  padding: 0 2px;
  line-height: 1;
  transition: color 0.15s;
}
.minimize-btn:hover { color: var(--text); }
.status-dot {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: var(--red-500);
  transition: background 0.3s, box-shadow 0.3s;
}
.status-dot.live {
  background: var(--green-500);
  box-shadow: 0 0 8px oklch(72.3% 0.219 149.58 / 0.5);
  animation: pulse 2s ease-in-out infinite;
}
@keyframes pulse {
  0%, 100% { opacity: 1; }
  50% { opacity: 0.6; }
}
.feed {
  flex: 1;
  overflow-y: auto;
  padding: 8px;
}
.feed::-webkit-scrollbar { width: 5px; }
.feed::-webkit-scrollbar-track { background: transparent; }
.feed::-webkit-scrollbar-thumb { background: oklch(40% 0.003 256 / 0.5); border-radius: 3px; }

.empty {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  height: 100%;
  color: var(--text-dim);
  gap: 8px;
}
.empty-icon { font-size: 28px; opacity: 0.4; }
.empty-text { font-size: 13px; }

.event {
  background: var(--card);
  border-radius: var(--radius-card);
  padding: 10px 12px;
  margin-bottom: 6px;
  cursor: pointer;
  transition: background 0.15s;
  border-left: 3px solid var(--zinc-600);
  animation: slideIn 0.2s ease-out;
}
.event:hover { background: var(--card-hover); }
.event.expanded { background: var(--card-hover); }

@keyframes slideIn {
  from { opacity: 0; transform: translateY(-8px); }
  to { opacity: 1; transform: translateY(0); }
}

.event-header {
  display: flex;
  align-items: center;
  gap: 8px;
}
.event-badge {
  font-size: 10px;
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.5px;
  padding: 2px 6px;
  border-radius: var(--radius-sm);
  color: var(--zinc-50);
  background: var(--zinc-600);
  white-space: nowrap;
}
.event-type {
  font-size: 12px;
  font-weight: 500;
  color: var(--text);
  flex: 1;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.event-time {
  font-size: 11px;
  color: var(--text-dim);
  white-space: nowrap;
}
.event-summary {
  font-size: 12px;
  color: var(--text-muted);
  margin-top: 4px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.event-channel {
  font-size: 10px;
  color: var(--text-dim);
  margin-top: 3px;
}
.event-json {
  margin-top: 8px;
  background: oklch(10% 0.003 256 / 0.5);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  padding: 10px;
  font-family: 'SF Mono', 'Menlo', monospace;
  font-size: 11px;
  line-height: 1.5;
  overflow-x: auto;
  max-height: 200px;
  overflow-y: auto;
  white-space: pre;
  -webkit-user-select: text;
  user-select: text;
  display: none;
}
.event.expanded .event-json { display: block; }
.event.expanded .event-summary { white-space: normal; }

.json-key { color: var(--json-key); }
.json-string { color: var(--json-string); }
.json-number { color: var(--json-number); }
.json-bool { color: var(--json-bool); }
.json-null { color: var(--json-null); }

.footer {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 10px 14px;
  border-top: 1px solid var(--border);
  font-size: 11px;
}
.footer-btn {
  background: oklch(23% 0.003 256 / 0.5);
  border: 1px solid oklch(32% 0.003 256 / 0.4);
  color: var(--text-muted);
  padding: 4px 10px;
  border-radius: var(--radius-sm);
  cursor: pointer;
  font-size: 11px;
  font-family: inherit;
  transition: all 0.15s;
}
.footer-btn:hover { background: oklch(32% 0.003 256 / 0.6); color: var(--text); border-color: oklch(40% 0.003 256 / 0.5); }
.footer-right { display: flex; gap: 6px; }

/* Source-specific colors — border-left uses 500, badge bg uses 700 */
.src-stripe { border-left-color: var(--purple-500); }
.src-stripe .event-badge { background: var(--purple-700); }
.src-github { border-left-color: var(--zinc-400); }
.src-github .event-badge { background: var(--zinc-700); }
.src-sentry { border-left-color: var(--indigo-500); }
.src-sentry .event-badge { background: var(--indigo-700); }
.src-shopify { border-left-color: var(--green-500); }
.src-shopify .event-badge { background: var(--green-700); }
.src-slack { border-left-color: var(--fuchsia-500); }
.src-slack .event-badge { background: var(--fuchsia-700); }
.src-discord { border-left-color: var(--indigo-500); }
.src-discord .event-badge { background: var(--indigo-700); }
.src-vercel { border-left-color: var(--zinc-400); }
.src-vercel .event-badge { background: var(--zinc-700); }
.src-twilio { border-left-color: var(--red-500); }
.src-twilio .event-badge { background: var(--red-700); }
.src-paypal { border-left-color: var(--sapphire-500); }
.src-paypal .event-badge { background: var(--sapphire-700); }
.src-linear { border-left-color: var(--violet-500); }
.src-linear .event-badge { background: var(--violet-700); }
.src-jira { border-left-color: var(--sapphire-500); }
.src-jira .event-badge { background: var(--sapphire-700); }
.src-pagerduty { border-left-color: var(--emerald-500); }
.src-pagerduty .event-badge { background: var(--emerald-700); }
.src-datadog { border-left-color: var(--purple-500); }
.src-datadog .event-badge { background: var(--purple-700); }
.src-grafana { border-left-color: var(--orange-500); }
.src-grafana .event-badge { background: var(--orange-700); }
.src-test { border-left-color: var(--amber-500); }
.src-test .event-badge { background: var(--amber-700); }
.src-webhook { border-left-color: var(--teal-500); }
.src-webhook .event-badge { background: var(--teal-700); }
.src-circleci { border-left-color: var(--green-500); }
.src-circleci .event-badge { background: var(--green-700); }
.src-gitlab { border-left-color: var(--orange-500); }
.src-gitlab .event-badge { background: var(--orange-700); }
.src-bitbucket { border-left-color: var(--sapphire-500); }
.src-bitbucket .event-badge { background: var(--sapphire-700); }
.src-heroku { border-left-color: var(--violet-500); }
.src-heroku .event-badge { background: var(--violet-700); }
.src-newrelic { border-left-color: var(--teal-500); }
.src-newrelic .event-badge { background: var(--teal-700); }
.src-sendgrid { border-left-color: var(--sky-500); }
.src-sendgrid .event-badge { background: var(--sky-700); }
.src-mailgun { border-left-color: var(--rose-500); }
.src-mailgun .event-badge { background: var(--rose-700); }
.src-zendesk { border-left-color: var(--emerald-500); }
.src-zendesk .event-badge { background: var(--emerald-700); }
.src-intercom { border-left-color: var(--sapphire-500); }
.src-intercom .event-badge { background: var(--sapphire-700); }
.src-hubspot { border-left-color: var(--orange-500); }
.src-hubspot .event-badge { background: var(--orange-700); }
</style>
</head>
<body>

<div class="header">
  <div class="header-left">
    <div class="logo">Dread</div>
    <div class="channel-count" id="channelCount"></div>
  </div>
  <div class="header-right">
    <div class="status">
      <span id="statusText">Connecting</span>
      <div class="status-dot" id="statusDot"></div>
    </div>
    <button class="minimize-btn" onclick="minimize()" title="Minimize to menu bar">&#x2013;</button>
  </div>
</div>

<div class="feed" id="feed">
  <div class="empty" id="empty">
    <div class="empty-icon">&#9678;</div>
    <div class="empty-text">Waiting for webhooks...</div>
  </div>
</div>

<div class="footer">
  <div class="footer-left">
    <span id="eventCount" style="color: var(--text-dim);">0 events</span>
  </div>
  <div class="footer-right">
    <button class="footer-btn" id="clearBtn" onclick="clearFeed()">Clear</button>
    <button class="footer-btn" id="pauseBtn" onclick="togglePause()">Pause</button>
  </div>
</div>

<script>
const SERVER_WS = "%%SERVER_WS%%";
const CHANNELS = %%CHANNELS_JSON%%;

const channelNames = {};
CHANNELS.forEach(c => { channelNames[c.id] = c.name; });

document.getElementById('channelCount').textContent = CHANNELS.length + ' channel' + (CHANNELS.length !== 1 ? 's' : '');

let ws = null;
let events = [];
let paused = false;
let reconnectTimer = null;

function connect() {
  const ids = CHANNELS.map(c => c.id).join(',');
  const url = SERVER_WS + '/ws?channels=' + ids;

  try { ws = new WebSocket(url); } catch(e) { scheduleReconnect(); return; }

  ws.onopen = () => {
    document.getElementById('statusDot').className = 'status-dot live';
    document.getElementById('statusText').textContent = 'Live';
  };

  ws.onclose = () => {
    document.getElementById('statusDot').className = 'status-dot';
    document.getElementById('statusText').textContent = 'Reconnecting';
    scheduleReconnect();
  };

  ws.onerror = () => { ws.close(); };

  ws.onmessage = (e) => {
    try {
      const msg = JSON.parse(e.data);
      if (msg.type === 'event' && msg.event) {
        addEvent(msg.event);
      }
    } catch(err) {}
  };
}

function scheduleReconnect() {
  if (reconnectTimer) return;
  reconnectTimer = setTimeout(() => {
    reconnectTimer = null;
    connect();
  }, 3000);
}

function addEvent(ev) {
  events.unshift(ev);
  if (events.length > 200) events = events.slice(0, 200);
  if (!paused) renderEvent(ev);
  updateCount();
}

function renderEvent(ev) {
  const feed = document.getElementById('feed');
  const empty = document.getElementById('empty');
  if (empty) empty.remove();

  const src = (ev.source || 'webhook').toLowerCase();
  const div = document.createElement('div');
  div.className = 'event src-' + src;
  div.onclick = () => div.classList.toggle('expanded');

  let jsonHtml = '';
  if (ev.raw_json) {
    try {
      const formatted = JSON.stringify(JSON.parse(ev.raw_json), null, 2);
      jsonHtml = syntaxHighlight(formatted);
    } catch(e) {
      jsonHtml = escapeHtml(ev.raw_json);
    }
  }

  const chName = channelNames[ev.channel] || ev.channel || '';

  div.innerHTML =
    '<div class="event-header">' +
      '<span class="event-badge">' + escapeHtml(src) + '</span>' +
      '<span class="event-type">' + escapeHtml(ev.type || 'event') + '</span>' +
      '<span class="event-time" data-ts="' + ev.timestamp + '">' + timeAgo(ev.timestamp) + '</span>' +
    '</div>' +
    '<div class="event-summary">' + escapeHtml(ev.summary || '') + '</div>' +
    (chName ? '<div class="event-channel">' + escapeHtml(chName) + '</div>' : '') +
    (jsonHtml ? '<div class="event-json">' + jsonHtml + '</div>' : '');

  feed.insertBefore(div, feed.firstChild);

  // Limit DOM nodes
  while (feed.children.length > 200) {
    feed.removeChild(feed.lastChild);
  }
}

function updateCount() {
  document.getElementById('eventCount').textContent = events.length + ' event' + (events.length !== 1 ? 's' : '');
}

function clearFeed() {
  events = [];
  const feed = document.getElementById('feed');
  feed.innerHTML = '<div class="empty" id="empty"><div class="empty-icon">&#9678;</div><div class="empty-text">Waiting for webhooks...</div></div>';
  updateCount();
}

function togglePause() {
  paused = !paused;
  document.getElementById('pauseBtn').textContent = paused ? 'Resume' : 'Pause';
  if (!paused) {
    // Re-render missed events
    const feed = document.getElementById('feed');
    feed.innerHTML = '';
    events.forEach(ev => renderEvent(ev));
  }
}

function timeAgo(ts) {
  if (!ts) return '';
  const diff = (Date.now() - new Date(ts).getTime()) / 1000;
  if (diff < 5) return 'now';
  if (diff < 60) return Math.floor(diff) + 's ago';
  if (diff < 3600) return Math.floor(diff / 60) + 'm ago';
  if (diff < 86400) return Math.floor(diff / 3600) + 'h ago';
  return Math.floor(diff / 86400) + 'd ago';
}

function escapeHtml(s) {
  const d = document.createElement('div');
  d.textContent = s;
  return d.innerHTML;
}

function syntaxHighlight(json) {
  json = json.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  return json.replace(/("(\\u[\da-fA-F]{4}|\\[^u]|[^\\"])*"(\s*:)?|\b(true|false|null)\b|-?\d+(?:\.\d*)?(?:[eE][+\-]?\d+)?)/g,
    function(m) {
      let c = 'json-number';
      if (/^"/.test(m)) { c = /:$/.test(m) ? 'json-key' : 'json-string'; }
      else if (/true|false/.test(m)) { c = 'json-bool'; }
      else if (/null/.test(m)) { c = 'json-null'; }
      return '<span class="' + c + '">' + m + '</span>';
    });
}

// Update timestamps every 30s
setInterval(() => {
  document.querySelectorAll('.event-time[data-ts]').forEach(el => {
    el.textContent = timeAgo(el.dataset.ts);
  });
}, 30000);

function minimize() {
  if (window.webkit && window.webkit.messageHandlers && window.webkit.messageHandlers.dread) {
    window.webkit.messageHandlers.dread.postMessage('minimize');
  }
}

// Connect
connect();
</script>
</body>
</html>`
