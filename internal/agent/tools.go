package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/Yerassyl20036/egov-web-crawler/internal/catalog"
	"github.com/Yerassyl20036/egov-web-crawler/internal/store"
)

// Catalog is the read side of the dataset store the tools need.
type Catalog interface {
	Search(ctx context.Context, p store.SearchParams) ([]store.SearchHit, error)
	Get(ctx context.Context, apiURI string) (catalog.Dataset, error)
	GetMany(ctx context.Context, apiURIs []string) (map[string]catalog.Dataset, error)
	Categories(ctx context.Context) ([]store.CategoryCount, error)
}

// RowPreviewer fetches sample rows from the portal's data API.
type RowPreviewer interface {
	PreviewRows(ctx context.Context, apiURI, version string, size int) ([]map[string]any, error)
}

// DatasetRef is the compact form of a dataset shown in the UI.
type DatasetRef struct {
	APIURI   string `json:"api_uri"`
	Title    string `json:"title"`
	Category string `json:"category,omitempty"`
	Agency   string `json:"agency,omitempty"`
	URL      string `json:"url"`
}

func refOf(d catalog.Dataset) DatasetRef {
	return DatasetRef{APIURI: d.APIURI, Title: d.Title(), Category: d.CategoryRu, Agency: d.AgencyRu, URL: d.URL()}
}

// ToolOutcome is what a tool call produced: the JSON sent back to the model
// plus a human-readable summary and dataset refs for the UI.
type ToolOutcome struct {
	Content  string
	IsError  bool
	Summary  string
	Datasets []DatasetRef
}

// Tools implements the agent's client-side tools.
type Tools struct {
	Catalog Catalog
	Rows    RowPreviewer // nil disables preview_rows
}

// Definitions returns the tool schemas sent to Claude. Requests stream with
// eager input streaming, so Execute validates every input itself.
func (t *Tools) Definitions() []anthropic.BetaToolUnionParam {
	defs := []anthropic.BetaToolParam{
		{
			Name: "search_datasets",
			Description: anthropic.String("Full-text search over the data.egov.kz catalog (about 4,000 dataset passports). " +
				"The index is Russian-first: pass Russian keywords (nouns and place names, not a full sentence) even if the user wrote in English or Kazakh. " +
				"Returns datasets ranked by relevance with api_uri, title, description, category, publishing agency, update cadence and portal URL. " +
				"Run several searches with synonyms or filters when the first results are weak."),
			InputSchema: anthropic.BetaToolInputSchemaParam{
				Properties: map[string]any{
					"query":    map[string]any{"type": "string", "description": "Russian keywords, e.g. 'поголовье скота Костанайская область'"},
					"category": map[string]any{"type": "string", "description": "Optional category filter (Russian or English name from list_categories), e.g. 'Здравоохранение'"},
					"agency":   map[string]any{"type": "string", "description": "Optional filter on the publishing agency's name, e.g. 'Акимат города Алматы' or 'Министерство финансов'"},
					"limit":    map[string]any{"type": "integer", "minimum": 1, "maximum": 20, "description": "Number of results, default 8"},
				},
				Required: []string{"query"},
			},
		},
		{
			Name: "get_dataset",
			Description: anthropic.String("Get the full passport of one dataset by api_uri: titles and descriptions in Russian/Kazakh/English, keywords, category, " +
				"publishing agency, owner, update cadence, whether it is up to date, creation and last-update dates, views, downloads and the portal URL."),
			InputSchema: anthropic.BetaToolInputSchemaParam{
				Properties: map[string]any{
					"api_uri": map[string]any{"type": "string", "description": "The dataset's api_uri from search_datasets"},
				},
				Required: []string{"api_uri"},
			},
		},
		{
			Name:        "list_categories",
			Description: anthropic.String("List the portal's dataset categories (Russian and English names) with dataset counts. Use it to choose a category filter or to describe what the catalog covers."),
			InputSchema: anthropic.BetaToolInputSchemaParam{Properties: map[string]any{}},
		},
	}
	if t.Rows != nil {
		defs = append(defs, anthropic.BetaToolParam{
			Name:        "preview_rows",
			Description: anthropic.String("Fetch the first rows of a dataset from the official data API to see its columns and sample values. Use only on datasets you already found."),
			InputSchema: anthropic.BetaToolInputSchemaParam{
				Properties: map[string]any{
					"api_uri": map[string]any{"type": "string", "description": "The dataset's api_uri"},
					"limit":   map[string]any{"type": "integer", "minimum": 1, "maximum": 10, "description": "Number of rows, default 5"},
				},
				Required: []string{"api_uri"},
			},
		})
	}
	out := make([]anthropic.BetaToolUnionParam, len(defs))
	for i := range defs {
		defs[i].EagerInputStreaming = anthropic.Bool(true)
		out[i] = anthropic.BetaToolUnionParam{OfTool: &defs[i]}
	}
	return out
}

var apiURIPattern = regexp.MustCompile(`^[A-Za-z0-9_\-]{1,200}$`)

type searchInput struct {
	Query    string `json:"query"`
	Category string `json:"category"`
	Agency   string `json:"agency"`
	Limit    int    `json:"limit"`
}

type datasetInput struct {
	APIURI string `json:"api_uri"`
	Limit  int    `json:"limit"`
}

// Execute runs one tool call. Inputs arrive unvalidated (eager input
// streaming), so they are decoded strictly and checked before use.
func (t *Tools) Execute(ctx context.Context, name string, rawInput []byte) ToolOutcome {
	switch name {
	case "search_datasets":
		var in searchInput
		if err := decodeStrict(rawInput, &in); err != nil {
			return invalidInput(rawInput, err)
		}
		in.Query = strings.TrimSpace(in.Query)
		if in.Query == "" {
			return errorOutcome("query must not be empty")
		}
		if in.Limit == 0 {
			in.Limit = 8
		}
		if in.Limit < 1 || in.Limit > 20 {
			return errorOutcome("limit must be between 1 and 20")
		}
		return t.search(ctx, in)
	case "get_dataset":
		var in datasetInput
		if err := decodeStrict(rawInput, &in); err != nil {
			return invalidInput(rawInput, err)
		}
		if !apiURIPattern.MatchString(in.APIURI) {
			return errorOutcome("api_uri must be the identifier returned by search_datasets")
		}
		return t.get(ctx, in.APIURI)
	case "list_categories":
		cats, err := t.Catalog.Categories(ctx)
		if err != nil {
			return errorOutcome("catalog unavailable: " + err.Error())
		}
		return ToolOutcome{Content: mustJSON(cats), Summary: fmt.Sprintf("%d categories", len(cats))}
	case "preview_rows":
		if t.Rows == nil {
			return errorOutcome("preview_rows is not enabled on this server")
		}
		var in datasetInput
		if err := decodeStrict(rawInput, &in); err != nil {
			return invalidInput(rawInput, err)
		}
		if !apiURIPattern.MatchString(in.APIURI) {
			return errorOutcome("api_uri must be the identifier returned by search_datasets")
		}
		if in.Limit == 0 {
			in.Limit = 5
		}
		if in.Limit < 1 || in.Limit > 10 {
			return errorOutcome("limit must be between 1 and 10")
		}
		rows, err := t.Rows.PreviewRows(ctx, in.APIURI, "v1", in.Limit)
		if err != nil {
			return errorOutcome("data API error: " + err.Error())
		}
		return ToolOutcome{Content: truncate(mustJSON(rows), 8000), Summary: fmt.Sprintf("%d sample rows", len(rows))}
	default:
		return errorOutcome("unknown tool " + name)
	}
}

type searchResult struct {
	APIURI        string  `json:"api_uri"`
	Title         string  `json:"title"`
	TitleKk       string  `json:"title_kk,omitempty"`
	Description   string  `json:"description,omitempty"`
	Category      string  `json:"category,omitempty"`
	Agency        string  `json:"agency,omitempty"`
	Actualization string  `json:"update_cadence,omitempty"`
	UpdatedAt     string  `json:"updated_at,omitempty"`
	URL           string  `json:"url"`
	Score         float64 `json:"score"`
}

func (t *Tools) search(ctx context.Context, in searchInput) ToolOutcome {
	hits, err := t.Catalog.Search(ctx, store.SearchParams{Query: in.Query, Category: in.Category, Agency: in.Agency, Limit: in.Limit})
	if errors.Is(err, store.ErrEmptyQuery) {
		return errorOutcome("the query has no searchable words; use Russian nouns or place names")
	}
	if err != nil {
		return errorOutcome("catalog unavailable: " + err.Error())
	}
	results := make([]searchResult, 0, len(hits))
	refs := make([]DatasetRef, 0, len(hits))
	for _, h := range hits {
		r := searchResult{
			APIURI:        h.APIURI,
			Title:         h.Title(),
			TitleKk:       truncate(h.TitleKk, 200),
			Description:   truncate(h.DescriptionRu, 300),
			Category:      h.CategoryRu,
			Agency:        h.AgencyRu,
			Actualization: catalog.ActualizationLabel(h.Actualization),
			URL:           h.URL(),
			Score:         float64(int(h.Score*1000)) / 1000,
		}
		if h.ModifiedAt != nil {
			r.UpdatedAt = h.ModifiedAt.Format("2006-01-02")
		}
		results = append(results, r)
		refs = append(refs, refOf(h.Dataset))
	}
	body := map[string]any{"query": in.Query, "count": len(results), "results": results}
	if len(results) == 0 {
		body["hint"] = "No matches. Try other Russian synonyms, a broader term, or drop the filters."
	}
	return ToolOutcome{Content: mustJSON(body), Summary: fmt.Sprintf("%d datasets", len(results)), Datasets: refs}
}

func (t *Tools) get(ctx context.Context, apiURI string) ToolOutcome {
	d, err := t.Catalog.Get(ctx, apiURI)
	if errors.Is(err, store.ErrNotFound) {
		return errorOutcome("no dataset with api_uri " + apiURI)
	}
	if err != nil {
		return errorOutcome("catalog unavailable: " + err.Error())
	}
	passport := map[string]any{
		"api_uri":        d.APIURI,
		"title_ru":       d.TitleRu,
		"title_kk":       d.TitleKk,
		"title_en":       d.TitleEn,
		"description_ru": truncate(d.DescriptionRu, 1500),
		"description_kk": truncate(d.DescriptionKk, 800),
		"description_en": truncate(d.DescriptionEn, 800),
		"keywords":       d.Keywords,
		"category":       d.CategoryRu,
		"category_en":    d.CategoryEn,
		"agency":         d.AgencyRu,
		"agency_en":      d.AgencyEn,
		"owner":          d.Owner,
		"update_cadence": catalog.ActualizationLabel(d.Actualization),
		"is_up_to_date":  d.IsActual,
		"views":          d.Views,
		"downloads":      d.Downloads,
		"url":            d.URL(),
	}
	if d.CreatedAt != nil {
		passport["created_at"] = d.CreatedAt.Format("2006-01-02")
	}
	if d.ModifiedAt != nil {
		passport["updated_at"] = d.ModifiedAt.Format("2006-01-02")
	}
	for k, v := range passport {
		if v == "" || v == nil {
			delete(passport, k)
		}
	}
	return ToolOutcome{Content: mustJSON(passport), Summary: d.Title(), Datasets: []DatasetRef{refOf(d)}}
}

func decodeStrict(raw []byte, v any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data after JSON object")
	}
	return nil
}

// invalidInput follows the eager-input-streaming convention: hand the raw
// input back as an error result so the model can retry the call.
func invalidInput(raw []byte, err error) ToolOutcome {
	return ToolOutcome{
		Content: mustJSON(map[string]string{"INVALID_JSON": string(raw), "error": err.Error()}),
		IsError: true,
		Summary: "invalid tool input",
	}
}

func errorOutcome(msg string) ToolOutcome {
	return ToolOutcome{Content: mustJSON(map[string]string{"error": msg}), IsError: true, Summary: msg}
}

func mustJSON(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Sprintf(`{"error":%q}`, err.Error())
	}
	return strings.TrimSpace(buf.String())
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
