// Package bcrypt provides a bcrypt-backed service.Hasher for service.HashField.
// It is a satellite of middleware/service: keeping the golang.org/x/crypto
// dependency isolated here lets the core service package stay standard-library
// only.
package bcrypt

import (
	xbcrypt "golang.org/x/crypto/bcrypt"

	"maniflex/middleware/service"
)

// Hasher returns a service.Hasher that bcrypt-hashes field values. An optional
// cost overrides bcrypt.DefaultCost.
//
//	service.HashField("password", bcrypt.Hasher())
//	service.HashField("password", bcrypt.Hasher(12))
func Hasher(cost ...int) service.Hasher {
	c := xbcrypt.DefaultCost
	if len(cost) > 0 {
		c = cost[0]
	}
	return func(plaintext string) (string, error) {
		hashed, err := xbcrypt.GenerateFromPassword([]byte(plaintext), c)
		if err != nil {
			return "", err
		}
		return string(hashed), nil
	}
}
