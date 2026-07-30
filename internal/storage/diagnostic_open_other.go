//go:build !linux

package storage

// Non-Linux platforms have no portable process-wide open-fd inventory. Keep
// legacy files rather than deleting an artifact that another worker may hold.
func diagnosticFileOpenBySameUID(string) (bool, error) {
	return true, nil
}
