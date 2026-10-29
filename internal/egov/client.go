// Package egov talks to data.egov.kz: the JSON listing endpoint used by the
// portal's own catalog page, the HTML dataset passport, and (with an API key)
// the row-level data API.
package egov

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Yerassyl20036/egov-web-crawler/internal/catalog"
)

// ErrNoAPIKey is returned by PreviewRows when the client has no API key.
var ErrNoAPIKey = errors.New("egov: EGOV_API_KEY is not set")

// Client is a small, polite HTTP client for data.egov.kz.
type Client struct {
	BaseURL   string
	APIKey    string
	UserAgent string
	HTTP      *http.Client
}

// NewClient returns a client with sane timeouts.
func NewClient(apiKey string) *Client {
	return &Client{
		BaseURL:   catalog.PortalBaseURL,
		APIKey:    apiKey,
		UserAgent: "ask-data-egov-kz/1.0 (+https://github.com/Yerassyl20036/egov-web-crawler)",
		HTTP:      &http.Client{Timeout: 30 * time.Second},
	}
}

// LocalizedName is the {nameRu, nameKk, nameEn} triple the portal uses
// for categories and government agencies.
type LocalizedName struct {
	ID     string `json:"id"`
	NameRu string `json:"nameRu"`
	NameKk string `json:"nameKk"`
	NameEn string `json:"nameEn"`
}

// ListedDataset is one entry of /datasets/getdatasetsre.
type ListedDataset struct {
	APIURI        string          `json:"apiUri"`
	NameRu        string          `json:"nameRu"`
	NameKk        string          `json:"nameKk"`
	NameEn        string          `json:"nameEn"`
	DescriptionRu string          `json:"descriptionRu"`
	DescriptionKk string          `json:"descriptionKk"`
	DescriptionEn string          `json:"descriptionEn"`
	KeyWords      []string        `json:"keyWords"`
	Status        string          `json:"status"`
	Views         int             `json:"views"`
	Downloads     int             `json:"downloads"`
	ActualizeType string          `json:"actualizeType"`
	Actual        *bool           `json:"actual"`
	CreatedDate   string          `json:"createdDate"`
	ModifiedDate  string          `json:"modifiedDate"`
	GovAgency     *LocalizedName  `json:"govAgency"`
	Categories    []LocalizedName `json:"categories"`
}

// ListResponse is the body of /datasets/getdatasetsre.
type ListResponse struct {
	ListSize int             `json:"listSize"`
	Datasets []ListedDataset `json:"datasets"`
}

// ListDatasets fetches one page of the public catalog, newest first.
func (c *Client) ListDatasets(ctx context.Context, page, count int) (*ListResponse, error) {
	params := url.Values{}
	params.Set("page", fmt.Sprint(page))
	params.Set("count", fmt.Sprint(count))
	params.Set("byGovAgencyId", "")
	params.Set("categoryId", "")
	params.Set("statusType", "")
	params.Set("datasetSortSelect", "createdDateDesc")
	params.Set("status", "")

	body, err := c.get(ctx, c.BaseURL+"/datasets/getdatasetsre?"+params.Encode())
	if err != nil {
		return nil, err
	}
	var resp ListResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("egov: decode listing page %d: %w", page, err)
	}
	return &resp, nil
}

// ToDataset converts a listing entry into the canonical model.
func (l ListedDataset) ToDataset(crawledAt time.Time) catalog.Dataset {
	d := catalog.Dataset{
		APIURI:        strings.TrimSpace(l.APIURI),
		TitleRu:       clean(l.NameRu),
		TitleKk:       clean(l.NameKk),
		TitleEn:       clean(l.NameEn),
		DescriptionRu: clean(l.DescriptionRu),
		DescriptionKk: clean(l.DescriptionKk),
		DescriptionEn: clean(l.DescriptionEn),
		Keywords:      cleanList(l.KeyWords),
		Actualization: l.ActualizeType,
		IsActual:      l.Actual,
		Status:        l.Status,
		Views:         l.Views,
		Downloads:     l.Downloads,
		CreatedAt:     ParsePortalTime(l.CreatedDate),
		ModifiedAt:    ParsePortalTime(l.ModifiedDate),
		CrawledAt:     crawledAt,
	}
	if len(l.Categories) > 0 {
		d.CategoryRu = clean(l.Categories[0].NameRu)
		d.CategoryKk = clean(l.Categories[0].NameKk)
		d.CategoryEn = clean(l.Categories[0].NameEn)
	}
	if l.GovAgency != nil {
		d.AgencyRu = clean(l.GovAgency.NameRu)
		d.AgencyKk = clean(l.GovAgency.NameKk)
		d.AgencyEn = clean(l.GovAgency.NameEn)
	}
	return d
}

// PreviewRows returns up to size rows of a dataset from the official data
// API (https://data.egov.kz/api/v4/{index}/{version}). It requires a free
// API key from the portal; without one the API answers 403.
func (c *Client) PreviewRows(ctx context.Context, apiURI, version string, size int) ([]map[string]any, error) {
	if c.APIKey == "" {
		return nil, ErrNoAPIKey
	}
	if version == "" {
		version = "v1"
	}
	params := url.Values{}
	params.Set("apiKey", c.APIKey)
	params.Set("source", fmt.Sprintf(`{"size":%d}`, size))
	endpoint := fmt.Sprintf("%s/api/v4/%s/%s?%s", c.BaseURL, url.PathEscape(apiURI), url.PathEscape(version), params.Encode())

	body, err := c.get(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	var rows []map[string]any
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("egov: decode rows of %s: %w", apiURI, err)
	}
	return rows, nil
}

func (c *Client) get(ctx context.Context, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Accept-Language", "ru")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("egov: GET %s: %w", redact(endpoint), err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("egov: read %s: %w", redact(endpoint), err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("egov: GET %s: status %d", redact(endpoint), resp.StatusCode)
	}
	return body, nil
}

// redact hides the API key in error messages and logs.
func redact(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	q := u.Query()
	if q.Has("apiKey") {
		q.Set("apiKey", "REDACTED")
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// ParsePortalTime parses the two date formats the portal uses:
// "Jan 29, 2026 10:27:30 AM" (JSON) and "29.01.2026 10:27" (passport page).
// Times are Astana local time (UTC+5).
func ParsePortalTime(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	for _, layout := range []string{"Jan 2, 2006 3:04:05 PM", "02.01.2006 15:04", "2006-01-02T15:04:05Z07:00"} {
		if t, err := time.ParseInLocation(layout, s, astana); err == nil {
			return &t
		}
	}
	return nil
}

var astana = time.FixedZone("Asia/Almaty", 5*60*60)

func clean(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func cleanList(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, s := range in {
		s = clean(s)
		key := strings.ToLower(s)
		if s == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, s)
	}
	return out
}
