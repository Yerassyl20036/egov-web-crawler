-- Dataset catalog of data.egov.kz with multilingual full-text search.
CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE TABLE IF NOT EXISTS datasets (
    api_uri         text PRIMARY KEY,
    title_ru        text NOT NULL DEFAULT '',
    title_kk        text NOT NULL DEFAULT '',
    title_en        text NOT NULL DEFAULT '',
    description_ru  text NOT NULL DEFAULT '',
    description_kk  text NOT NULL DEFAULT '',
    description_en  text NOT NULL DEFAULT '',
    keywords        text[] NOT NULL DEFAULT '{}',
    -- array_to_string is not IMMUTABLE, so the generated column below reads
    -- this flattened copy of keywords maintained by the application.
    keywords_text   text NOT NULL DEFAULT '',
    category_ru     text NOT NULL DEFAULT '',
    category_kk     text NOT NULL DEFAULT '',
    category_en     text NOT NULL DEFAULT '',
    agency_ru       text NOT NULL DEFAULT '',
    agency_kk       text NOT NULL DEFAULT '',
    agency_en       text NOT NULL DEFAULT '',
    owner           text NOT NULL DEFAULT '',
    actualization   text NOT NULL DEFAULT '',
    is_actual       boolean,
    status          text NOT NULL DEFAULT '',
    views           integer NOT NULL DEFAULT 0,
    downloads       integer NOT NULL DEFAULT 0,
    created_at      timestamptz,
    modified_at     timestamptz,
    crawled_at      timestamptz NOT NULL DEFAULT now(),

    -- Two search vectors, ranked separately so long descriptions can't
    -- drown out short, precise titles:
    --   head_vec: A = title, B = keywords + publishing agency, D = category
    --   body_vec: descriptions
    -- Many titles repeat verbatim across regions ("Перечень государственных
    -- больниц в разрезе регионов"), so the agency (usually a regional akimat)
    -- carries the region and gets weight B. Russian text uses the Russian
    -- snowball stemmer; Kazakh has no stemmer in Postgres, so it uses
    -- 'simple'; English fields use 'english'.
    head_vec tsvector GENERATED ALWAYS AS (
        setweight(to_tsvector('russian', title_ru), 'A') ||
        setweight(to_tsvector('simple', title_kk), 'A') ||
        setweight(to_tsvector('english', title_en), 'A') ||
        setweight(to_tsvector('russian', keywords_text), 'B') ||
        setweight(to_tsvector('simple', keywords_text), 'B') ||
        setweight(to_tsvector('russian', agency_ru), 'B') ||
        setweight(to_tsvector('simple', agency_kk), 'B') ||
        setweight(to_tsvector('english', agency_en), 'B') ||
        setweight(to_tsvector('russian', category_ru), 'D') ||
        setweight(to_tsvector('simple', category_kk), 'D') ||
        setweight(to_tsvector('english', category_en), 'D')
    ) STORED,
    body_vec tsvector GENERATED ALWAYS AS (
        to_tsvector('russian', description_ru) ||
        to_tsvector('simple', description_kk) ||
        to_tsvector('english', description_en)
    ) STORED
);

CREATE INDEX IF NOT EXISTS datasets_head_idx ON datasets USING gin (head_vec);
CREATE INDEX IF NOT EXISTS datasets_body_idx ON datasets USING gin (body_vec);
CREATE INDEX IF NOT EXISTS datasets_title_ru_trgm ON datasets USING gin (title_ru gin_trgm_ops);
CREATE INDEX IF NOT EXISTS datasets_title_kk_trgm ON datasets USING gin (title_kk gin_trgm_ops);
CREATE INDEX IF NOT EXISTS datasets_agency_ru_trgm ON datasets USING gin (agency_ru gin_trgm_ops);
CREATE INDEX IF NOT EXISTS datasets_category_ru_idx ON datasets (category_ru);
