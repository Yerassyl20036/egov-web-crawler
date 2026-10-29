package egov

import (
	"bytes"
	"context"
	"strings"

	"golang.org/x/net/html"

	"github.com/Yerassyl20036/egov-web-crawler/internal/catalog"
)

// Passport is the metadata table ("паспорт набора данных") shown on a
// dataset page. The listing JSON already carries most of it; the passport
// adds the owner and the data/meta links.
type Passport struct {
	Name              string   `json:"name,omitempty"`
	Description       string   `json:"description,omitempty"`
	Owner             string   `json:"owner,omitempty"`
	Category          string   `json:"category,omitempty"`
	GovAgency         string   `json:"gov_agency,omitempty"`
	Keywords          []string `json:"keywords,omitempty"`
	ActualizationType string   `json:"actualization_type,omitempty"`
	ActualityStatus   string   `json:"actuality_status,omitempty"`
	CreationDate      string   `json:"creation_date,omitempty"`
	RenewalDate       string   `json:"renewal_date,omitempty"`
	LinkToData        string   `json:"link_to_data,omitempty"`
	LinkToMetaInfo    string   `json:"link_to_meta_info,omitempty"`
	Status            string   `json:"status,omitempty"`
}

// The portal renders the passport in Russian, Kazakh or English depending on
// the visitor's language cookie, so rows are matched by label in all three.
var passportLabels = map[string]string{
	"название": "name", "атауы": "name", "name": "name",
	"описание": "description", "сипаттамасы": "description", "description": "description",
	"владелец": "owner", "иесі": "owner", "owner": "owner",
	"категория": "category", "category": "category",
	"государственный орган": "gov_agency", "государственные органы": "gov_agency",
	"мемлекеттік орган": "gov_agency", "мемлекеттік органдар": "gov_agency",
	"government agency": "gov_agency", "gov agency": "gov_agency",
	"ключевые слова": "keywords", "түйінді сөздер": "keywords", "keywords": "keywords",
	"тип актуализации": "actualization_type", "өзектендіру түрі": "actualization_type", "actualization type": "actualization_type",
	"статус актуальности": "actuality_status", "өзектілік мәртебесі": "actuality_status", "actuality status": "actuality_status",
	"дата создания": "creation_date", "дата размещения": "creation_date", "құру күні": "creation_date", "creation date": "creation_date",
	"дата обновления": "renewal_date", "жаңарту күні": "renewal_date", "renewal date": "renewal_date",
	"ссылка на данные": "link_to_data", "деректерге сілтеме": "link_to_data", "link to the data": "link_to_data",
	"ссылка на мета информацию": "link_to_meta_info", "мета ақпаратқа сілтеме": "link_to_meta_info", "link to meta-information": "link_to_meta_info",
	"статус": "status", "мәртебесі": "status", "status": "status",
}

// ParsePassport extracts the passport table from a dataset page.
func ParsePassport(page []byte) (Passport, error) {
	root, err := html.Parse(bytes.NewReader(page))
	if err != nil {
		return Passport{}, err
	}
	var p Passport
	table := findByID(root, "metaTab")
	if table == nil {
		table = root
	}
	for _, row := range findAll(table, "tr") {
		cells := children(row, "td")
		if len(cells) < 2 {
			continue
		}
		field, ok := passportLabels[strings.ToLower(textOf(cells[0]))]
		if !ok {
			continue
		}
		if field == "keywords" {
			for _, li := range findAll(cells[1], "li") {
				if kw := textOf(li); kw != "" {
					p.Keywords = append(p.Keywords, kw)
				}
			}
			continue
		}
		p.set(field, textOf(cells[1]))
	}
	return p, nil
}

// FetchPassport downloads and parses the passport of one dataset.
func (c *Client) FetchPassport(ctx context.Context, apiURI string) (Passport, error) {
	page, err := c.get(ctx, catalog.DatasetURL(apiURI))
	if err != nil {
		return Passport{}, err
	}
	return ParsePassport(page)
}

func (p *Passport) set(field, value string) {
	if value == "" {
		return
	}
	target := map[string]*string{
		"name":               &p.Name,
		"description":        &p.Description,
		"owner":              &p.Owner,
		"category":           &p.Category,
		"gov_agency":         &p.GovAgency,
		"actualization_type": &p.ActualizationType,
		"actuality_status":   &p.ActualityStatus,
		"creation_date":      &p.CreationDate,
		"renewal_date":       &p.RenewalDate,
		"link_to_data":       &p.LinkToData,
		"link_to_meta_info":  &p.LinkToMetaInfo,
		"status":             &p.Status,
	}[field]
	if target != nil && *target == "" {
		*target = value
	}
}

func findByID(n *html.Node, id string) *html.Node {
	if n.Type == html.ElementNode {
		for _, a := range n.Attr {
			if a.Key == "id" && a.Val == id {
				return n
			}
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findByID(c, id); found != nil {
			return found
		}
	}
	return nil
}

func findAll(n *html.Node, tag string) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == tag {
			out = append(out, n)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return out
}

func children(n *html.Node, tag string) []*html.Node {
	var out []*html.Node
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && c.Data == tag {
			out = append(out, c)
		}
	}
	return out
}

func textOf(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
			b.WriteByte(' ')
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return clean(b.String())
}
