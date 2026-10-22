FROM node:22 AS frontend
WORKDIR /src/web
ENV CI=true
COPY web/package.json web/pnpm-lock.yaml ./
RUN corepack pnpm install --frozen-lockfile
COPY web/ ./
RUN corepack pnpm build

FROM frontend AS frontend-development
RUN chmod -R a+rwX /src/web/node_modules

FROM golang:1.26.10 AS backend
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY db/ ./db/
COPY web/embed.go ./web/embed.go
COPY --from=frontend /src/web/dist/ ./web/dist/
RUN CGO_ENABLED=0 go build -trimpath -o /out/astovia ./cmd/astovia \
    && mkdir -p /out/data

FROM backend AS development
RUN GOBIN=/usr/local/bin go install github.com/air-verse/air@v1.61.1 \
    && chmod -R a+rwX /go/pkg/mod \
    && mkdir -p /src/.air-tmp \
    && chmod a+rwx /src/.air-tmp
ENV GOCACHE=/tmp/go-build
COPY .air.toml ./.air.toml
CMD ["air", "-c", ".air.toml"]

FROM scratch
COPY --from=backend /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=backend /out/astovia /astovia
COPY --from=backend --chown=1000:1000 /out/data/ /app/data/
WORKDIR /app
USER 1000:1000
EXPOSE 8080
ENTRYPOINT ["/astovia"]
CMD ["serve"]
