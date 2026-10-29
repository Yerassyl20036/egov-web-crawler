# One image for the API (with the built web UI) and the CLI tools:
# api (default), seed, crawler, eval.

FROM node:22-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 app
WORKDIR /app
COPY --from=build /out/ /usr/local/bin/
COPY --from=web /web/dist ./web
COPY data/datasets.jsonl.gz ./data/
COPY evals ./evals
ENV PORT=8080 WEB_DIR=/app/web
USER app
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- http://127.0.0.1:8080/api/health >/dev/null || exit 1
CMD ["api"]
