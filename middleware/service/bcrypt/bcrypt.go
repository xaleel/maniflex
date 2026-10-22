// Package bcrypt provides a bcrypt-backed service.Hasher for service.HashField.
// It is a satellite of middleware/service: keeping the golang.org/x/crypto
// dependency isolated here lets the core service package stay standard-library
// only.
package bcrypt

import (
	"errors"
	"fmt"

	xbcrypt "golang.org/x/crypto/bcrypt"

	"github.com/xaleel/maniflex/middleware/service"
)

// Hasher returns a service.Hasher that bcrypt-hashes field values. An optional
// cost overrides bcrypt.DefaultCost and must be in [bcrypt.MinCost,
// bcrypt.MaxCost] — 4 to 31.
//
//	service.HashField("password", bcrypt.Hasher())
//	service.HashField("password", bcrypt.Hasher(12))
//
// A cost outside that range panics here rather than failing later. It used to be
// passed straight through, and the two ways that went wrong were both silent:
// Hasher(32) compiled and then answered 500 to every create and update, because
// GenerateFromPassword rejects the cost per call and nothing had checked it; and
// Hasher(2) quietly became DefaultCost, so an operator who had thought about the
// number got one they did not choose (audit AUTH-15).
//
// bcrypt refuses a password longer than 72 bytes. That error is wrapped in
// service.ErrClientInput so HashField answers 400 rather than 500 — it is the
// caller's value, not a server fault. Note the limit is in **bytes**, which no
// mfx:"maxlen:" can express: that tag counts characters, so a 25-character CJK
// passphrase is 75 bytes and over the line.
func Hasher(cost ...int) service.Hasher {
	c := xbcrypt.DefaultCost
	if len(cost) > 0 {
		c = cost[0]
		if c < xbcrypt.MinCost || c > xbcrypt.MaxCost {
			panic(fmt.Sprintf("bcrypt.Hasher: cost %d is outside the allowed range %d..%d; "+
				"%d is the default and a sensible starting point",
				c, xbcrypt.MinCost, xbcrypt.MaxCost, xbcrypt.DefaultCost))
		}
	}
	return func(plaintext string) (string, error) {
		hashed, err := xbcrypt.GenerateFromPassword([]byte(plaintext), c)
		if err != nil {
			if errors.Is(err, xbcrypt.ErrPasswordTooLong) {
				return "", fmt.Errorf("%w: %w", service.ErrClientInput, err)
			}
			return "", err
		}
		return string(hashed), nil
	}
}
