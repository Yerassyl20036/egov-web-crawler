package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"golang.org/x/net/html"
)

type Queue struct {
	totalQueued int
	number      int
	elements    []string
	mu          sync.Mutex
}

func (q *Queue) enqueue(url string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.elements = append(q.elements, url)
	q.totalQueued++
	q.number++
}
func (q *Queue) dequeue() string {
	q.mu.Lock()
	defer q.mu.Unlock()
	url := q.elements[0]
	q.elements = q.elements[1:]
	q.number--
	return url
}
func (q *Queue) size() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.number
}

type CrawledSet struct {
	data   map[uint64]bool
	number int
	mu     sync.Mutex
}

func (c *CrawledSet) add(url string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data[hashUrl(url)] = true
	c.number++
}
func (c *CrawledSet) contains(url string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.data[hashUrl(url)]
}
func (c *CrawledSet) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.number
}
func hashUrl(url string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(url))
	return h.Sum64()
}
func getHref(t html.Token) (ok bool, href string) {
	for _, a := range t.Attr {
		if a.Key == "href" {
			if len(a.Val) == 0 || !strings.HasPrefix(a.Val, "http") {
				ok = false
				href = a.Val
				return ok, href
			}
			href = a.Val
			ok = true
		}
	}
	return ok, href
}
func fetchPage(url string, c chan []byte) {
	res, err := http.Get(url)
	if err != nil {
		body := []byte("")
		c <- body
		return
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		body = []byte("")
	}
	c <- body
}
func parseHTML(currUrl string, content []byte, q *Queue, crawled *CrawledSet, db *DatabaseConnection) {
	z := html.NewTokenizer(bytes.NewReader(content))
	tokenCount := 0
	pageContentLength := 0
	body := false
	webpage := Webpage{Url: currUrl, Title: "", Content: ""}
	for {
		if z.Next() == html.ErrorToken || tokenCount > 500 {
			if crawled.size() < 1000 {
				db.insertWebpage(webpage)
			}
			return
		}
		t := z.Token()
		if t.Type == html.StartTagToken {
			if t.Data == "body" {
				body = true
			}
			if t.Data == "javascript" || t.Data == "script" || t.Data == "style" {
				// Skip script and style tags
				z.Next()
				continue
			}
			if t.Data == "title" {
				z.Next()
				title := z.Token().Data // data disappears after z.Token() is called
				webpage.Title = title
				fmt.Printf("Count: %d | %s -> %s\n", crawled.size(), currUrl, title)
			}
			if t.Data == "a" {
				ok, href := getHref(t)
				if !ok {
					continue
				}
				if crawled.contains(href) {
					// Already crawled
					continue
				} else {
					q.enqueue(href)
				}
			}
		}
		if body && t.Type == html.TextToken && pageContentLength < 500 {
			webpage.Content += strings.TrimSpace(t.Data)
			pageContentLength += len(t.Data)
		}
		tokenCount++
	}
}

// parseEgovListPage parses the list page and extracts dataset links
func parseEgovListPage(content []byte, baseUrl string) []string {
	links := make([]string, 0)
	z := html.NewTokenizer(bytes.NewReader(content))

	for {
		tokenType := z.Next()
		if tokenType == html.ErrorToken {
			break
		}

		t := z.Token()
		if t.Type == html.StartTagToken && t.Data == "a" {
			for _, attr := range t.Attr {
				if attr.Key == "href" && strings.Contains(attr.Val, "/datasets/view?index=") {
					fullUrl := attr.Val
					if !strings.HasPrefix(fullUrl, "http") {
						fullUrl = "https://data.egov.kz" + attr.Val
					}
					links = append(links, fullUrl)
				}
			}
		}
	}

	return links
}

// fetchEgovAPIPage fetches datasets from the egov.kz JSON API
func fetchEgovAPIPage(page int) (*EgovAPIResponse, error) {
	baseUrl := "https://data.egov.kz/datasets/getdatasetsre"
	params := url.Values{}
	params.Add("page", fmt.Sprintf("%d", page))
	params.Add("count", "20")
	params.Add("byGovAgencyId", "")
	params.Add("categoryId", "")
	params.Add("statusType", "")
	params.Add("datasetSortSelect", "createdDateDesc")
	params.Add("status", "")

	fullUrl := baseUrl + "?" + params.Encode()

	resp, err := http.Get(fullUrl)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var apiResp EgovAPIResponse
	err = json.Unmarshal(body, &apiResp)
	if err != nil {
		return nil, err
	}

	return &apiResp, nil
}

// parseEgovDetailPage parses the detail page of a dataset
func parseEgovDetailPage(url string, content []byte) Dataset {
	dataset := Dataset{
		Url:       url,
		CrawledAt: time.Now(),
	}

	z := html.NewTokenizer(bytes.NewReader(content))
	inTitle := false
	inDescription := false

	for {
		tokenType := z.Next()
		if tokenType == html.ErrorToken {
			break
		}

		t := z.Token()

		// Parse title
		if t.Type == html.StartTagToken {
			if t.Data == "h1" || t.Data == "h2" {
				for _, attr := range t.Attr {
					if attr.Key == "class" && strings.Contains(attr.Val, "title") {
						inTitle = true
					}
				}
			}
			if t.Data == "div" || t.Data == "p" {
				for _, attr := range t.Attr {
					if attr.Key == "class" && strings.Contains(attr.Val, "description") {
						inDescription = true
					}
				}
			}
		}

		if t.Type == html.TextToken {
			text := strings.TrimSpace(t.Data)
			if inTitle && len(text) > 0 {
				dataset.Title = text
				inTitle = false
			}
			if inDescription && len(text) > 0 {
				dataset.Description += text + " "
			}
		}

		if t.Type == html.EndTagToken {
			if (t.Data == "div" || t.Data == "p") && inDescription {
				inDescription = false
			}
		}
	}

	dataset.Description = strings.TrimSpace(dataset.Description)
	return dataset
}

type DatabaseConnection struct {
	access            bool
	uri               string
	client            *mongo.Client
	webpageCollection *mongo.Collection
	datasetCollection *mongo.Collection
}

func (d *DatabaseConnection) connect() {
	if d.access {
		// Connect to database
		d.uri = os.Getenv("MONGODB_URI")
		client, err := mongo.Connect(context.TODO(), options.Client().ApplyURI(d.uri))
		if err != nil {
			panic(err)
		}
		d.client = client
		d.webpageCollection = d.client.Database("webCrawlerArchive").Collection("webpages")
		d.datasetCollection = d.client.Database("webCrawlerArchive").Collection("datasets")
		filter := bson.D{{}}
		// Deletes all documents in the collection
		d.webpageCollection.DeleteMany(context.TODO(), filter)
		d.datasetCollection.DeleteMany(context.TODO(), filter)
	}
}
func (d *DatabaseConnection) disconnect() {
	if d.access {
		d.client.Disconnect(context.TODO())
	}
}
func (d *DatabaseConnection) insertWebpage(webpage Webpage) {
	if d.access {
		d.webpageCollection.InsertOne(context.TODO(), webpage)
	}
}
func (d *DatabaseConnection) insertDataset(dataset Dataset) {
	if d.access {
		d.datasetCollection.InsertOne(context.TODO(), dataset)
	}
}

type Webpage struct {
	Url     string
	Title   string
	Content string
}

type Dataset struct {
	Url         string    `bson:"url"`
	Title       string    `bson:"title"`
	Description string    `bson:"description"`
	Date        string    `bson:"date"`
	Views       int       `bson:"views"`
	Downloads   int       `bson:"downloads"`
	Comments    int       `bson:"comments"`
	Status      string    `bson:"status"`
	Owner       string    `bson:"owner"`
	Categories  []string  `bson:"categories"`
	ApiUri      string    `bson:"api_uri"`
	CrawledAt   time.Time `bson:"crawled_at"`

	// Detailed passport information
	PassportName              string   `bson:"passport_name"`
	PassportDescription       string   `bson:"passport_description"`
	PassportOwner             string   `bson:"passport_owner"`
	PassportCategory          string   `bson:"passport_category"`
	PassportGovAgency         string   `bson:"passport_gov_agency"`
	PassportKeywords          []string `bson:"passport_keywords"`
	PassportActualizationType string   `bson:"passport_actualization_type"`
	PassportActualityStatus   string   `bson:"passport_actuality_status"`
	PassportCreationDate      string   `bson:"passport_creation_date"`
	PassportRenewalDate       string   `bson:"passport_renewal_date"`
	PassportLinkToData        string   `bson:"passport_link_to_data"`
	PassportLinkToMetaInfo    string   `bson:"passport_link_to_meta_info"`
	PassportStatus            string   `bson:"passport_status"`
	PassportVersion           string   `bson:"passport_version"`
}

type EgovDatasetResponse struct {
	ApiUri        string `json:"apiUri"`
	NameRu        string `json:"nameRu"`
	NameKk        string `json:"nameKk"`
	NameEn        string `json:"nameEn"`
	DescriptionRu string `json:"descriptionRu"`
	DescriptionKk string `json:"descriptionKk"`
	DescriptionEn string `json:"descriptionEn"`
	CreatedDate   string `json:"createdDate"`
	Views         int    `json:"views"`
	Downloads     int    `json:"downloads"`
	CommentsCount int    `json:"commentsCount"`
	Status        string `json:"status"`
}

type EgovAPIResponse struct {
	Datasets []EgovDatasetResponse `json:"datasets"`
	ListSize int                   `json:"listSize"`
}

func main() {
	webArchiveAccess := true
	if godotenv.Load() != nil {
		fmt.Println("Error loading .env file. No access to web archive.")
		webArchiveAccess = false
	}
	db := DatabaseConnection{access: webArchiveAccess, uri: "", client: nil}
	db.connect()

	crawled := CrawledSet{data: make(map[uint64]bool)}

	fmt.Println("Starting egov.kz dataset crawler...")
	fmt.Println("Fetching datasets via API...")

	// Worker pool configuration
	numWorkers := 10 // Number of concurrent workers for fetching detail pages
	jobs := make(chan Dataset, 100)
	results := make(chan Dataset, 100)
	var wg sync.WaitGroup

	// Start worker pool
	for w := 1; w <= numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for dataset := range jobs {
				// Fetch detailed passport information from the detail page
				passport, err := ParseDetailPage(dataset.Url)
				if err != nil {
					fmt.Printf("      [Worker %d] Warning: Failed to parse %s: %v\n", workerID, dataset.Url, err)
				} else {
					// Add passport information to dataset
					dataset.PassportName = passport.Name
					dataset.PassportDescription = passport.Description
					dataset.PassportOwner = passport.Owner
					dataset.PassportCategory = passport.Category
					dataset.PassportGovAgency = passport.GovAgency
					dataset.PassportKeywords = passport.Keywords
					dataset.PassportActualizationType = passport.ActualizationType
					dataset.PassportActualityStatus = passport.ActualityStatus
					dataset.PassportCreationDate = passport.CreationDate
					dataset.PassportRenewalDate = passport.RenewalDate
					dataset.PassportLinkToData = passport.LinkToData
					dataset.PassportLinkToMetaInfo = passport.LinkToMetaInfo
					dataset.PassportStatus = passport.Status
					dataset.PassportVersion = passport.Version
				}
				results <- dataset
			}
		}(w)
	}

	// Goroutine to close results channel when all workers are done
	go func() {
		wg.Wait()
		close(results)
	}()

	// Goroutine to save results to database
	var saveWg sync.WaitGroup
	saveWg.Add(1)
	savedCount := 0
	go func() {
		defer saveWg.Done()
		for dataset := range results {
			db.insertDataset(dataset)
			crawled.add(dataset.Url)
			savedCount++
			if savedCount%10 == 0 {
				fmt.Printf("      ✓ Saved %d datasets to database\n", savedCount)
			}
		}
	}()

	totalDatasets := 0
	page := 0
	maxPages := 500 // Set higher than 388 to ensure we get all pages

	for page < maxPages {
		fmt.Printf("\nFetching API page %d...\n", page)

		apiResp, err := fetchEgovAPIPage(page)
		if err != nil {
			fmt.Printf("Error fetching page %d: %v\n", page, err)
			break
		}

		if apiResp.ListSize == 0 || len(apiResp.Datasets) == 0 {
			fmt.Printf("No more datasets found on page %d\n", page)
			break
		}

		fmt.Printf("Found %d datasets on page %d (total so far: %d)\n",
			len(apiResp.Datasets), page, totalDatasets+len(apiResp.Datasets))

		for _, ds := range apiResp.Datasets {
			datasetUrl := fmt.Sprintf("https://data.egov.kz/datasets/view?index=%s", ds.ApiUri)

			if crawled.contains(datasetUrl) {
				continue
			}

			// Convert API response to Dataset struct
			dataset := Dataset{
				Url:         datasetUrl,
				Title:       ds.NameRu,
				Description: ds.DescriptionRu,
				Date:        ds.CreatedDate,
				Views:       ds.Views,
				Downloads:   ds.Downloads,
				Comments:    ds.CommentsCount,
				Status:      ds.Status,
				ApiUri:      ds.ApiUri,
				CrawledAt:   time.Now(),
			}

			// Use fallback titles if Russian is empty
			if dataset.Title == "" {
				if ds.NameKk != "" {
					dataset.Title = ds.NameKk
				} else if ds.NameEn != "" {
					dataset.Title = ds.NameEn
				} else {
					dataset.Title = "No title"
				}
			}

			// Use fallback descriptions
			if dataset.Description == "" {
				if ds.DescriptionKk != "" {
					dataset.Description = ds.DescriptionKk
				} else if ds.DescriptionEn != "" {
					dataset.Description = ds.DescriptionEn
				}
			}

			fmt.Printf("  [%d] %s\n", totalDatasets+1, dataset.Title)

			// Send dataset to worker pool for detail page fetching
			jobs <- dataset
			totalDatasets++
		}

		page++

		// Small delay between API pages
		time.Sleep(300 * time.Millisecond)
	}

	// Close jobs channel to signal workers to finish
	close(jobs)

	fmt.Println("\nWaiting for all workers to finish...")
	saveWg.Wait()

	db.disconnect()
	fmt.Println("\n------------------CRAWLER STATS------------------")
	fmt.Printf("Total API pages fetched: %d\n", page)
	fmt.Printf("Total datasets crawled: %d\n", savedCount)
}

type CrawlerStats struct {
	pagesPerMinute        string // 0 0 \n 1 100
	crawledRatioPerMinute string
	startTime             time.Time
}

func (c *CrawlerStats) update(crawled *CrawledSet, queue *Queue, t time.Time) {
	c.pagesPerMinute += fmt.Sprintf("%f %d\n", t.Sub(c.startTime).Minutes(), crawled.size())
	c.crawledRatioPerMinute += fmt.Sprintf("%f %f\n", t.Sub(c.startTime).Minutes(), float64(crawled.size())/float64(queue.size()))
}
func (c *CrawlerStats) print() {
	fmt.Println("Pages crawled per minute:")
	fmt.Println(c.pagesPerMinute)
	fmt.Println("Crawl to Queued Ratio per minute:")
	fmt.Println(c.crawledRatioPerMinute)
}
