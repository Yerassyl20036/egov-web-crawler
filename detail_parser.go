package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// DatasetPassport contains detailed passport information from the detail page
type DatasetPassport struct {
	Name              string    `bson:"name"`
	Description       string    `bson:"description"`
	Owner             string    `bson:"owner"`
	Category          string    `bson:"category"`
	GovAgency         string    `bson:"gov_agency"`
	Keywords          []string  `bson:"keywords"`
	ActualizationType string    `bson:"actualization_type"`
	ActualityStatus   string    `bson:"actuality_status"`
	CreationDate      string    `bson:"creation_date"`
	RenewalDate       string    `bson:"renewal_date"`
	LinkToData        string    `bson:"link_to_data"`
	LinkToMetaInfo    string    `bson:"link_to_meta_info"`
	Status            string    `bson:"status"`
	Version           string    `bson:"version"`
	CrawledAt         time.Time `bson:"crawled_at"`
}

// fetchDetailPage fetches the HTML content of a dataset detail page
func fetchDetailPage(detailUrl string) ([]byte, error) {
	resp, err := http.Get(detailUrl)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch page: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("non-200 status code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	return body, nil
}

// parsePassportTable parses the passport table from the detail page HTML
func parsePassportTable(content []byte) DatasetPassport {
	passport := DatasetPassport{
		CrawledAt: time.Now(),
	}

	z := html.NewTokenizer(bytes.NewReader(content))

	var currentFieldName string
	captureNextText := false
	inKeywordSpan := false
	keywords := make([]string, 0)

	for {
		tokenType := z.Next()
		if tokenType == html.ErrorToken {
			break
		}

		token := z.Token()

		// Look for table cells that might contain field names or values
		if token.Type == html.StartTagToken {
			if token.Data == "td" || token.Data == "th" {
				// Prepare to capture the text content
				captureNextText = true
			}

			// Check for keyword spans
			if token.Data == "span" {
				for _, attr := range token.Attr {
					if attr.Key == "class" && strings.Contains(attr.Val, "keyword") {
						inKeywordSpan = true
					}
				}
			}
		}

		if token.Type == html.TextToken && captureNextText {
			text := strings.TrimSpace(token.Data)

			if len(text) == 0 {
				continue
			}

			// Handle keywords specially
			if inKeywordSpan {
				keywords = append(keywords, text)
				inKeywordSpan = false
				continue
			}

			// Identify field names and capture the next value
			switch {
			case strings.Contains(text, "Название") || strings.Contains(text, "Name"):
				currentFieldName = "name"
			case strings.Contains(text, "Описание") || strings.Contains(text, "Description"):
				currentFieldName = "description"
			case strings.Contains(text, "Владелец") || strings.Contains(text, "Owner"):
				currentFieldName = "owner"
			case strings.Contains(text, "Категория") || strings.Contains(text, "Category"):
				currentFieldName = "category"
			case strings.Contains(text, "Государственный орган") || strings.Contains(text, "Gov Agency"):
				currentFieldName = "gov_agency"
			case strings.Contains(text, "Ключевые слова") || strings.Contains(text, "Keywords"):
				currentFieldName = "keywords"
			case strings.Contains(text, "Тип актуализации") || strings.Contains(text, "Actualization type"):
				currentFieldName = "actualization_type"
			case strings.Contains(text, "Статус актуальности") || strings.Contains(text, "Actuality status"):
				currentFieldName = "actuality_status"
			case strings.Contains(text, "Дата размещения") || strings.Contains(text, "Creation date"):
				currentFieldName = "creation_date"
			case strings.Contains(text, "Дата обновления") || strings.Contains(text, "Renewal date"):
				currentFieldName = "renewal_date"
			case strings.Contains(text, "Ссылка на данные") || strings.Contains(text, "Link to the data"):
				currentFieldName = "link_to_data"
			case strings.Contains(text, "Ссылка на мета информацию") || strings.Contains(text, "Link to meta-information"):
				currentFieldName = "link_to_meta_info"
			case strings.Contains(text, "Статус") && currentFieldName == "": // Only match if not already in a field
				currentFieldName = "status"
			case strings.Contains(text, "Версия") || strings.Contains(text, "Version"):
				currentFieldName = "version"
			default:
				// This might be the value for the current field
				if currentFieldName != "" && !isFieldName(text) {
					assignFieldValue(&passport, currentFieldName, text)
					currentFieldName = "" // Reset after capturing value
				}
			}
		}

		if token.Type == html.EndTagToken && (token.Data == "td" || token.Data == "th") {
			captureNextText = false
		}
	}

	// Assign collected keywords
	if len(keywords) > 0 {
		passport.Keywords = keywords
	}

	return passport
}

// isFieldName checks if the text is likely a field name rather than a value
func isFieldName(text string) bool {
	fieldNames := []string{
		"Название", "Name",
		"Описание", "Description",
		"Владелец", "Owner",
		"Категория", "Category",
		"Государственный орган", "Gov Agency",
		"Ключевые слова", "Keywords",
		"Тип актуализации", "Actualization type",
		"Статус актуальности", "Actuality status",
		"Дата размещения", "Creation date",
		"Дата обновления", "Renewal date",
		"Ссылка на данные", "Link to the data",
		"Ссылка на мета информацию", "Link to meta-information",
		"Статус", "Status",
		"Версия", "Version",
	}

	for _, fieldName := range fieldNames {
		if strings.Contains(text, fieldName) {
			return true
		}
	}
	return false
}

// assignFieldValue assigns a value to the appropriate field in the passport
func assignFieldValue(passport *DatasetPassport, fieldName, value string) {
	switch fieldName {
	case "name":
		if passport.Name == "" {
			passport.Name = value
		}
	case "description":
		if passport.Description == "" {
			passport.Description = value
		}
	case "owner":
		if passport.Owner == "" {
			passport.Owner = value
		}
	case "category":
		if passport.Category == "" {
			passport.Category = value
		}
	case "gov_agency":
		if passport.GovAgency == "" {
			passport.GovAgency = value
		}
	case "actualization_type":
		if passport.ActualizationType == "" {
			passport.ActualizationType = value
		}
	case "actuality_status":
		if passport.ActualityStatus == "" {
			passport.ActualityStatus = value
		}
	case "creation_date":
		if passport.CreationDate == "" {
			passport.CreationDate = value
		}
	case "renewal_date":
		if passport.RenewalDate == "" {
			passport.RenewalDate = value
		}
	case "link_to_data":
		if passport.LinkToData == "" {
			passport.LinkToData = value
		}
	case "link_to_meta_info":
		if passport.LinkToMetaInfo == "" {
			passport.LinkToMetaInfo = value
		}
	case "status":
		if passport.Status == "" {
			passport.Status = value
		}
	case "version":
		if passport.Version == "" {
			passport.Version = value
		}
	}
}

// ParseDetailPage fetches and parses a dataset detail page
func ParseDetailPage(detailUrl string) (*DatasetPassport, error) {
	content, err := fetchDetailPage(detailUrl)
	if err != nil {
		return nil, err
	}

	passport := parsePassportTable(content)
	return &passport, nil
}
