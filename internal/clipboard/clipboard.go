package clipboard

// Copy copies text to the system clipboard.
// Returns an error if the clipboard is not available.
func Copy(text string) error {
	return copy(text)
}
