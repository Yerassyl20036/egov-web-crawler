package catalog

import (
	"bufio"
	"compress/gzip"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// WriteSnapshot writes datasets as JSON lines, gzip-compressed when the path
// ends in .gz. Rows are sorted by api_uri so snapshots diff cleanly.
func WriteSnapshot(path string, datasets []Dataset) error {
	sorted := append([]Dataset(nil), datasets...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].APIURI < sorted[j].APIURI })

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var w io.Writer = f
	var gz *gzip.Writer
	if strings.HasSuffix(path, ".gz") {
		gz, _ = gzip.NewWriterLevel(f, gzip.BestCompression)
		w = gz
	}
	buf := bufio.NewWriter(w)
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	for _, d := range sorted {
		if err := enc.Encode(d); err != nil {
			return err
		}
	}
	if err := buf.Flush(); err != nil {
		return err
	}
	if gz != nil {
		if err := gz.Close(); err != nil {
			return err
		}
	}
	return f.Close()
}

// ReadFile loads datasets from a snapshot (.jsonl / .jsonl.gz) or from the
// legacy MongoDB CSV export of the original crawler (.csv).
func ReadFile(path string) ([]Dataset, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		r = gz
	}
	if strings.HasSuffix(strings.TrimSuffix(path, ".gz"), ".csv") {
		return readLegacyCSV(r)
	}
	return readJSONL(r)
}

func readJSONL(r io.Reader) ([]Dataset, error) {
	var out []Dataset
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for line := 1; sc.Scan(); line++ {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var d Dataset
		if err := json.Unmarshal(sc.Bytes(), &d); err != nil {
			return nil, fmt.Errorf("snapshot line %d: %w", line, err)
		}
		out = append(out, d)
	}
	return out, sc.Err()
}

// readLegacyCSV maps the columns of datasets_export.csv (MongoDB export of
// the original crawler, Feb 2026) onto the canonical model. That export only
// has Russian text and no keywords.
func readLegacyCSV(r io.Reader) ([]Dataset, error) {
	cr := csv.NewReader(r)
	cr.LazyQuotes = true
	header, err := cr.Read()
	if err != nil {
		return nil, err
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.TrimSpace(h)] = i
	}
	get := func(rec []string, name string) string {
		if i, ok := col[name]; ok && i < len(rec) {
			return strings.Join(strings.Fields(rec[i]), " ")
		}
		return ""
	}

	seen := map[string]bool{}
	var out []Dataset
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		uri := get(rec, "api_uri")
		if uri == "" || seen[uri] {
			continue
		}
		seen[uri] = true
		d := Dataset{
			APIURI:        uri,
			TitleRu:       get(rec, "title"),
			DescriptionRu: get(rec, "description"),
			CategoryRu:    get(rec, "passport_category"),
			AgencyRu:      get(rec, "passport_gov_agency"),
			Owner:         get(rec, "passport_owner"),
			Actualization: get(rec, "passport_actualization_type"),
			Status:        get(rec, "status"),
			CreatedAt:     parseTime(get(rec, "date"), "Jan 2, 2006 3:04:05 PM"),
			ModifiedAt:    parseTime(get(rec, "passport_renewal_date"), "02.01.2006 15:04"),
		}
		d.Views, _ = strconv.Atoi(get(rec, "views"))
		d.Downloads, _ = strconv.Atoi(get(rec, "downloads"))
		if v := get(rec, "passport_actuality_status"); v != "" {
			actual := v == "Да"
			d.IsActual = &actual
		}
		if t := parseTime(get(rec, "crawled_at"), time.RFC3339); t != nil {
			d.CrawledAt = *t
		}
		out = append(out, d)
	}
	return out, nil
}

func parseTime(s, layout string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.ParseInLocation(layout, s, time.FixedZone("Asia/Almaty", 5*60*60))
	if err != nil {
		return nil
	}
	return &t
}
