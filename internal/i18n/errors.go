package i18n

import (
	"errors"
	"fmt"
)

type LocalizedError struct {
	Key   string
	Data  map[string]any
	Cause error
}

func (e *LocalizedError) Error() string {
	if e.Cause != nil {
		return e.Key + ": " + e.Cause.Error()
	}
	return e.Key
}

func (e *LocalizedError) Unwrap() error {
	return e.Cause
}

func NewError(key string, data map[string]any, cause error) error {
	return &LocalizedError{
		Key:   key,
		Data:  data,
		Cause: cause,
	}
}

func LocalizeError(m *Manager, err error) string {
	if err == nil {
		return ""
	}

	var le *LocalizedError
	if errors.As(err, &le) {
		msg := m.T(le.Key, le.Data)
		if le.Cause != nil {
			return fmt.Sprintf("%s: %s", msg, LocalizeError(m, le.Cause))
		}
		return msg
	}

	return err.Error()
}
