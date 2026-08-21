package i18n

import (
	"errors"
	"fmt"
	"sync"

	goi18n "github.com/nicksnyder/go-i18n/v2/i18n"
	"github.com/pelletier/go-toml/v2"
	"golang.org/x/text/language"
)

type LocalizedError struct {
	Key   string
	Data  map[string]any
	Cause error
}

var (
	defaultLocalizer *goi18n.Localizer
	defaultOnce      sync.Once
)

// englishLocalizer renders localization keys as readable English text. It is
// used as the default Error() output so library consumers see meaningful
// messages without constructing a Manager themselves.
func englishLocalizer() *goi18n.Localizer {
	defaultOnce.Do(func() {
		bundle := goi18n.NewBundle(language.English)
		bundle.RegisterUnmarshalFunc("toml", toml.Unmarshal)
		_, _ = bundle.LoadMessageFileFS(localeFS, "locales/active.en.toml")
		defaultLocalizer = goi18n.NewLocalizer(bundle, "en")
	})
	return defaultLocalizer
}

func (e *LocalizedError) Error() string {
	msg, err := englishLocalizer().Localize(&goi18n.LocalizeConfig{
		MessageID:    e.Key,
		TemplateData: e.Data,
	})
	if err != nil {
		msg = e.Key
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s", msg, e.Cause.Error())
	}
	return msg
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
