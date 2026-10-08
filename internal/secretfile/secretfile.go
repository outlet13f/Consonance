// Package secretfile reads secrets mounted as files.
package secretfile

import (
	"fmt"
	"os"
	"strings"
)

// Read returns the trimmed contents of a secret file. An empty secret is an error: an empty
// token would make an API answer 401 to everyone, and an empty password would create a login
// role without one.
func Read(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	secret := strings.TrimSpace(string(b))
	if secret == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return secret, nil
}
