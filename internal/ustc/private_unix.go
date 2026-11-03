//go:build !windows

package ustc

import "os"

func securePrivatePath(path string) error {
	return os.Chmod(path, 0600)
}

func securePrivateDirectory(path string) error {
	return os.Chmod(path, 0700)
}
