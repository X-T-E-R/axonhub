//go:build !linux

package maintenance

import "errors"

func clearStaleSocket(string) error { return errors.New("local maintenance socket requires Linux") }
