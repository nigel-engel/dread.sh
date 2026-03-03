package notify

// DefaultSound is the default notification sound.
// macOS: system sound name. Linux: freedesktop sound name.
const DefaultSound = "Glass"

// Send sends a desktop notification with the given title, body, and sound.
// Pass an empty string for sound to use the default.
// Falls back silently if notifications aren't available.
func Send(title, body, sound string) {
	if sound == "" {
		sound = DefaultSound
	}
	send(title, body, sound)
}
