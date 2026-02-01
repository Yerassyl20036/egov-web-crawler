# eGov.kz Dataset Web Crawler

## Overview
This web crawler is specifically designed to scrape dataset information from Kazakhstan's government open data portal (data.egov.kz). It's written in Go and uses MongoDB to store the scraped data.

## Features

- Scrapes dataset listings from data.egov.kz with pagination support
- Extracts detailed information from individual dataset pages
- Stores data in MongoDB for easy access and analysis
- Handles deduplication automatically
- Respectful crawling with built-in delays

## Prerequisites

- Go 1.21 or higher
- Docker and Docker Compose
- MongoDB (can be run via Docker)

## Setup

1. **Install Go dependencies**
   ```bash
   go mod download
   ```

2. **Set up environment variables**
   ```bash
   cp .env.example .env
   ```
   
   Edit `.env` if needed to change MongoDB credentials.

3. **Start MongoDB using Docker**
   ```bash
   docker-compose up -d
   ```

   This will start MongoDB on port 27017 with:
   - Username: `admin`
   - Password: `password`
   - Database: `webCrawlerArchive`

## Running the Crawler

```bash
go run main.go
```

The crawler will:
1. Fetch all pages from the dataset listing (with pagination)
2. Extract links to individual dataset pages
3. Scrape detailed information from each dataset page
4. Store the results in MongoDB

## MongoDB Collections

- **datasets**: Stores scraped dataset information including title, description, URL, etc.
- **webpages**: Legacy collection for general web crawling

## Viewing the Data

You can connect to MongoDB to view the scraped data:

```bash
# Connect to MongoDB shell
docker exec -it web-crawler-mongodb mongosh -u admin -p password --authenticationDatabase admin

# In the MongoDB shell
use webCrawlerArchive
db.datasets.find().pretty()
db.datasets.countDocuments()
```

Or use MongoDB Compass with connection string:
```
mongodb://admin:password@localhost:27017/webCrawlerArchive?authSource=admin
```

## Docker Commands

```bash
# Start MongoDB
docker-compose up -d

# Stop MongoDB
docker-compose down

# View logs
docker-compose logs -f

# Stop and remove data
docker-compose down -v
```

## Configuration

You can adjust the crawler behavior in `main.go`:
- `maxPages`: Maximum number of list pages to crawl (default: 50)
- Delays between requests (currently 1 second for list pages, 500ms for detail pages)

## Project Structure

```
.
├── main.go              # Main crawler implementation & API fetching
├── detail_parser.go     # Detail page passport parser
├── go.mod               # Go module dependencies
├── Dockerfile           # MongoDB Docker configuration
├── docker-compose.yml   # Docker Compose setup
├── .env                 # Environment variables (not in git)
├── .env.example         # Example environment file
├── start.sh             # Helper script to start everything
└── README.md            # This file
```

## How it Works

The crawler operates in two phases:

1. **Phase 1: Fetch Datasets via API**
   - Uses the egov.kz API endpoint (`/datasets/getdatasetsre`) to fetch dataset listings
   - Automatically handles pagination (up to 500 pages)
   - Extracts basic dataset information (title, description, views, downloads, etc.)

2. **Phase 2: Parse Detailed Passport Information**
   - For each dataset, visits the detail page (e.g., `https://data.egov.kz/datasets/view?index=...`)
   - Scrapes the passport table containing metadata:
     - Owner (Владелец)
     - Category (Категория)
     - Government Agency (Государственный орган)
     - Keywords (Ключевые слова)
     - Actualization Type (Тип актуализации)
     - Actuality Status (Статус актуальности)
     - Creation/Renewal dates
     - API and Meta-information links
     - Status and Version
   - Stores comprehensive dataset information in MongoDB

## Data Structure

Each dataset document in MongoDB contains:
- **Basic Info**: URL, title, description, creation date
- **Statistics**: Views, downloads, comments count
- **Passport Metadata**: Owner, category, government agency, keywords, actualization details
- **API Links**: Direct links to data and meta-information
- **Timestamps**: When the data was crawled

## Notes

- The crawler is designed to be respectful with built-in delays
- Duplicate URLs are automatically filtered
- The crawler will stop pagination when it encounters empty pages
- All timestamps are stored in UTC



