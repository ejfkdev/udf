package i18n

import (
	"embed"
	"os"
	"strings"

	goi18n "github.com/nicksnyder/go-i18n/v2/i18n"
	"github.com/pelletier/go-toml/v2"
	"golang.org/x/text/language"
)

//go:embed locales/*.toml
var localeFS embed.FS

type Manager struct {
	bundle    *goi18n.Bundle
	localizer *goi18n.Localizer
	tag       string
}

func New(lang string) (*Manager, error) {
	bundle := goi18n.NewBundle(language.English)
	bundle.RegisterUnmarshalFunc("toml", toml.Unmarshal)
	if _, err := bundle.LoadMessageFileFS(localeFS, "locales/active.en.toml"); err != nil {
		return nil, err
	}
	if _, err := bundle.LoadMessageFileFS(localeFS, "locales/active.zh.toml"); err != nil {
		return nil, err
	}

	tag := normalizeLang(lang)
	localizer := goi18n.NewLocalizer(bundle, tag, "en")
	return &Manager{bundle: bundle, localizer: localizer, tag: tag}, nil
}

func DetectLanguage(args []string) string {
	if lang := parseLangArg(args); lang != "" {
		return normalizeLang(lang)
	}
	for _, key := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if value := os.Getenv(key); value != "" {
			return normalizeLang(value)
		}
	}
	return "en"
}

func (m *Manager) T(id string, data map[string]any) string {
	msg, err := m.localizer.Localize(&goi18n.LocalizeConfig{
		MessageID:    id,
		TemplateData: data,
	})
	if err != nil {
		return id
	}
	return msg
}

func (m *Manager) Lang() string {
	return m.tag
}

func parseLangArg(args []string) string {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "--lang=") {
			return strings.TrimPrefix(arg, "--lang=")
		}
		if arg == "--lang" || arg == "-l" {
			if i+1 < len(args) {
				return args[i+1]
			}
		}
	}
	return ""
}

func normalizeLang(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "en"
	}
	value = strings.Split(value, ".")[0]
	value = strings.ReplaceAll(value, "-", "_")
	lower := strings.ToLower(value)
	switch {
	case strings.HasPrefix(lower, "zh"):
		return "zh"
	case strings.HasPrefix(lower, "en"):
		return "en"
	default:
		return "en"
	}
}
