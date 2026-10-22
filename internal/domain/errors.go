package domain

import (
	"errors"
	"fmt"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrQuotaExceeded = errors.New("quota exceeded")
	ErrAlreadyExists = errors.New("already exists")
)

type ErrorKind int

const (
	KindInvalid ErrorKind = iota + 1
	KindNotFound
	KindConflict
	KindTooLarge
	KindQuotaExceeded
	KindUnsupported
)

type Error struct {
	Kind    ErrorKind
	Message string
}

func (e *Error) Error() string {
	return e.Message
}

func newError(kind ErrorKind, format string, args ...any) error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, args...)}
}

func Invalid(format string, args ...any) error {
	return newError(KindInvalid, format, args...)
}

func NotFound(format string, args ...any) error {
	return newError(KindNotFound, format, args...)
}

func Conflict(format string, args ...any) error {
	return newError(KindConflict, format, args...)
}

func TooLarge(format string, args ...any) error {
	return newError(KindTooLarge, format, args...)
}

func QuotaExceeded(format string, args ...any) error {
	return newError(KindQuotaExceeded, format, args...)
}

func Unsupported(format string, args ...any) error {
	return newError(KindUnsupported, format, args...)
}
