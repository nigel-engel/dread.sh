package notify

// Send sends a desktop notification with the given title and body.
// Falls back silently if notifications aren't available.
func Send(title, body string) {
	send(title, body)
}
