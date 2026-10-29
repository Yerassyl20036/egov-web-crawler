# One image for the API (with the built web UI) and the CLI tools:
# api (default), seed, crawler, eval.
#
# The build stages run on the build machine's platform and cross-compile, so
#   docker buildx build --platform linux/amd64 .
# on an ARM laptop is fast (no emulated npm/go builds).

FROM --platform=$BUILDPLATFORM node:22-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

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
# Shell form so $PORT is expanded: PaaS hosts such as Render set their own port.
HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- "http://127.0.0.1:${PORT}/api/health" >/dev/null || exit 1
CMD ["api"]
