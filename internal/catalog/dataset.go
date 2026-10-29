// Package catalog holds the canonical dataset model shared by the crawler,
// the Postgres store, the agent tools and the evals.
package catalog

import (
	"strings"
	"time"
)

// PortalBaseURL is the public open-data portal of Kazakhstan.
const PortalBaseURL = "https://data.egov.kz"

// Dataset is one dataset "passport" from data.egov.kz.
// Text fields are kept in all three portal languages when available
// (ru = Russian, kk = Kazakh, en = English); most datasets only have ru/kk.
type Dataset struct {
	APIURI string `json:"api_uri"`

	TitleRu       string `json:"title_ru"`
	TitleKk       string `json:"title_kk,omitempty"`
	TitleEn       string `json:"title_en,omitempty"`
	DescriptionRu string `json:"description_ru,omitempty"`
	DescriptionKk string `json:"description_kk,omitempty"`
	DescriptionEn string `json:"description_en,omitempty"`

	Keywords []string `json:"keywords,omitempty"`

	CategoryRu string `json:"category_ru,omitempty"`
	CategoryKk string `json:"category_kk,omitempty"`
	CategoryEn string `json:"category_en,omitempty"`

	AgencyRu string `json:"agency_ru,omitempty"`
	AgencyKk string `json:"agency_kk,omitempty"`
	AgencyEn string `json:"agency_en,omitempty"`

	Owner string `json:"owner,omitempty"`

	// Actualization is the update cadence as a portal code (e.g. ANNUALLY)
	// or, for legacy CSV rows, the Russian label (e.g. "Ежегодно").
	Actualization string `json:"actualization,omitempty"`
	IsActual      *bool  `json:"is_actual,omitempty"`
	Status        string `json:"status,omitempty"`

	Views     int `json:"views"`
	Downloads int `json:"downloads"`

	CreatedAt  *time.Time `json:"created_at,omitempty"`
	ModifiedAt *time.Time `json:"modified_at,omitempty"`
	CrawledAt  time.Time  `json:"crawled_at"`
}

// URL is the human-facing page of the dataset on the portal.
func (d Dataset) URL() string { return DatasetURL(d.APIURI) }

// DatasetURL builds the portal page URL for an api_uri.
func DatasetURL(apiURI string) string {
	return PortalBaseURL + "/datasets/view?index=" + apiURI
}

// Title returns the best available title, preferring Russian.
func (d Dataset) Title() string {
	return firstNonEmpty(d.TitleRu, d.TitleKk, d.TitleEn)
}

// ActualizationLabel renders the update cadence in Russian, which is the
// language the agent answers about the catalog in.
func ActualizationLabel(code string) string {
	if label, ok := actualizationLabels[strings.ToUpper(code)]; ok {
		return label
	}
	return code
}

var actualizationLabels = map[string]string{
	"AS_NEEDED":              "По мере необходимости",
	"DAILY":                  "Ежедневно",
	"WEEKLY":                 "Еженедельно",
	"MONTHLY":                "Ежемесячно",
	"QUARTERLY":              "Ежеквартально",
	"ONCE_EVERY_SIX_MONTHS":  "1 раз в полгода",
	"TWO_TIMES_A_YEAR":       "2 раза в год",
	"YEARLY":                 "Ежегодно",
	"CONSTANTLY":             "Постоянно",
	"ONCE_EVERY_THREE_YEARS": "1 раз в 3 года",
	"ONCE_EVERY_FIVE_YEARS":  "1 раз в 5 лет",
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
