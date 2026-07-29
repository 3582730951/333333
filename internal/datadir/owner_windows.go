//go:build windows

package datadir

import "os"

func verifyOwner(os.FileInfo) error { return nil }

func verifyCredentialOwner(os.FileInfo) error { return nil }
