package core

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"
)

type Error struct {
	Code   string
	Detail string
	Status int
}

func (e *Error) Error() string { return e.Detail }

func NewError(code, detail string, status int) *Error {
	return &Error{Code: code, Detail: detail, Status: status}
}

func Conflict(code, detail string) *Error { return NewError(code, detail, 409) }

func Unavailable(code, detail string) *Error { return NewError(code, detail, 503) }

func NewID() string {
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%016x%s", time.Now().UnixNano(), hex.EncodeToString(random))
}

func RandomToken(bytes int) string {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(value)
}

func NowMS() int64 { return time.Now().UnixMilli() }

func ISOTime(milliseconds *int64) *string {
	if milliseconds == nil || *milliseconds == 0 {
		return nil
	}
	value := time.UnixMilli(*milliseconds).UTC().Format("2006-01-02T15:04:05.000000Z")
	return &value
}
