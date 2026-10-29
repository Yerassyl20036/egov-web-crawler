package egov

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParsePassportKazakhPage(t *testing.T) {
	page, err := os.ReadFile("testdata/passport_kk.html")
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParsePassport(page)
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string][2]string{
		"category":    {p.Category, "Мемлекеттік сектор"},
		"gov_agency":  {p.GovAgency, "АО Отбасы банк"},
		"created":     {p.CreationDate, "29.01.2026 10:27"},
		"actualizing": {p.ActualizationType, "Тоқсан сайын"},
		"status":      {p.Status, "Жарияланған"},
	}
	for name, c := range checks {
		if c[0] != c[1] {
			t.Errorf("%s = %q, want %q", name, c[0], c[1])
		}
	}
	if !strings.HasPrefix(p.Name, "Шымкент қаласы") {
		t.Errorf("name = %q", p.Name)
	}
	if len(p.Keywords) != 2 || p.Keywords[1] != "список" {
		t.Errorf("keywords = %q", p.Keywords)
	}
	if p.Owner != "" {
		t.Errorf("owner should be empty on this page, got %q", p.Owner)
	}
}

func TestListDatasetsToDataset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/datasets/getdatasetsre" || r.URL.Query().Get("page") != "3" {
			t.Errorf("unexpected request %s", r.URL)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"listSize": 1,
			"datasets": []map[string]any{{
				"apiUri":        "hospitals_almaty",
				"nameRu":        "  Список   больниц ",
				"nameKk":        "Ауруханалар тізімі",
				"keyWords":      []string{"больницы", "Больницы", " "},
				"actualizeType": "YEARLY",
				"createdDate":   "Jan 29, 2026 10:27:30 AM",
				"views":         7,
				"govAgency":     map[string]string{"nameRu": "Акимат города Алматы", "nameEn": "Akimat of Almaty"},
				"categories":    []map[string]string{{"nameRu": "Здравоохранение", "nameEn": "Healthcare"}},
			}},
		})
	}))
	defer srv.Close()

	c := NewClient("")
	c.BaseURL = srv.URL
	resp, err := c.ListDatasets(context.Background(), 3, 20)
	if err != nil {
		t.Fatal(err)
	}
	d := resp.Datasets[0].ToDataset(time.Unix(0, 0))
	if d.TitleRu != "Список больниц" || d.CategoryEn != "Healthcare" || d.AgencyRu != "Акимат города Алматы" {
		t.Errorf("unexpected dataset %+v", d)
	}
	if len(d.Keywords) != 1 {
		t.Errorf("keywords should be de-duplicated, got %q", d.Keywords)
	}
	if d.CreatedAt == nil || d.CreatedAt.Year() != 2026 || d.CreatedAt.Hour() != 10 {
		t.Errorf("created_at = %v", d.CreatedAt)
	}
}

func TestPreviewRowsNeedsKey(t *testing.T) {
	if _, err := NewClient("").PreviewRows(context.Background(), "x", "v1", 5); err != ErrNoAPIKey {
		t.Fatalf("err = %v, want ErrNoAPIKey", err)
	}
}

func TestRedactHidesAPIKey(t *testing.T) {
	got := redact("https://data.egov.kz/api/v4/x/v1?apiKey=secret123&source=%7B%7D")
	if strings.Contains(got, "secret123") {
		t.Fatalf("api key leaked: %s", got)
	}
}
