// Package welcome is the starter landing page and the "why these patterns"
// page (the original template-welcome). Safe to delete: see README.md.
package welcome

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"

	"github.com/jayleaton/go-fast-stack/apps/web/ui/i18n"
)

// Copy is the page text for one locale. It lives in copy/<locale>.json
// rather than the shared messages so the whole folder can be deleted.
type Copy struct {
	Hero struct {
		Badge, Title, Subtitle, Primary, Secondary, Skip string
	} `json:"hero"`
	What struct {
		Eyebrow, Title, Body string
		Cards                []TitleBody `json:"cards"`
	} `json:"what"`
	Start struct {
		Eyebrow, Title, Subtitle string
		Steps                    []TitleBody `json:"steps"`
	} `json:"start"`
	Patterns struct {
		Eyebrow, Title, Subtitle, More string
		Items                          []struct {
			Title, Body, Path string
		} `json:"items"`
	} `json:"patterns"`
	Later struct {
		Eyebrow, Title string
		Items          []TitleBody `json:"items"`
	} `json:"later"`
	Remove struct {
		Eyebrow, Title, Body string
		Steps                []string `json:"steps"`
		Note                 string
	} `json:"remove"`
	Footer struct {
		// Label names the footer nav landmark.
		Label, Why, Login, Signup string
	} `json:"footer"`
	Why struct {
		Eyebrow, Title, Subtitle, IntroTitle, IntroBody string
		Items                                           []struct {
			Title, Analogy, LookHere, Path string
		} `json:"items"`
		Back, Start string
	} `json:"why"`
}

// TitleBody is a card or step.
type TitleBody struct {
	Title, Body string
}

//go:embed copy/*.json
var copyFS embed.FS

var copies = func() map[i18n.Locale]*Copy {
	out := map[i18n.Locale]*Copy{}
	for _, locale := range i18n.Locales {
		raw, err := copyFS.ReadFile("copy/" + string(locale) + ".json")
		if err != nil {
			panic(fmt.Sprintf("welcome: read %s: %v", locale, err))
		}
		content := new(Copy)
		if err := json.Unmarshal(raw, content); err != nil {
			panic(fmt.Sprintf("welcome: parse %s: %v", locale, err))
		}
		out[locale] = content
	}
	return out
}()

// CopyFor returns the copy for the request locale.
func CopyFor(ctx context.Context) *Copy {
	if content, ok := copies[i18n.From(ctx).Locale]; ok {
		return content
	}
	return copies[i18n.DefaultLocale]
}
