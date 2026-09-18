package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

const (
	argonTime    = 3
	argonMemory  = 65536
	argonThreads = 1
	argonSalt    = 16
	argonHash    = 32
)

func hashPassword(password string) (string, error) {
	salt := make([]byte, argonSalt)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonHash)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonTime, argonThreads, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash)), nil
}

func verifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" {
		return false
	}
	parameters := map[string]int{}
	for _, pair := range strings.Split(parts[3], ",") {
		values := strings.SplitN(pair, "=", 2)
		if len(values) != 2 {
			return false
		}
		value, err := strconv.Atoi(values[1])
		if err != nil {
			return false
		}
		parameters[values[0]] = value
	}
	memory, timeCost, threads := parameters["m"], parameters["t"], parameters["p"]
	if memory <= 0 || timeCost <= 0 || threads <= 0 || threads > 255 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(expected) == 0 {
		return false
	}
	actual := argon2.IDKey([]byte(password), salt, uint32(timeCost), uint32(memory), uint8(threads), uint32(len(expected)))
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

func validatePassword(password string) error {
	if utf8.RuneCountInString(password) < 15 || utf8.RuneCountInString(password) > 128 {
		return errors.New("administrator password must contain 15-128 characters")
	}
	return nil
}
