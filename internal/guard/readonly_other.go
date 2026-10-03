//go:build !linux

package guard

// ReadOnly always reports false outside Linux, where the agent doesn't run.
func ReadOnly(string) (bool, error) { return false, nil }
