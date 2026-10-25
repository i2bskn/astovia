# Build stages run on the build platform and cross-compile for the target, so
# multi-platform builds need no emulation.
FROM --platform=$BUILDPLATFORM node:22 AS frontend
WORKDIR /src/web
ENV CI=true
COPY web/package.json web/pnpm-lock.yaml ./
RUN corepack pnpm install --frozen-lockfile
COPY web/ ./
RUN corepack pnpm build

FROM frontend AS frontend-development
RUN chmod -R a+rwX /src/web/node_modules

FROM --platform=$BUILDPLATFORM golang:1.26.10 AS backend
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY db/ ./db/
COPY web/embed.go ./web/embed.go
COPY --from=frontend /src/web/dist/ ./web/dist/
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /out/astovia ./cmd/astovia \
    && mkdir -p /out/data

FROM backend AS development
RUN GOBIN=/usr/local/bin go install github.com/air-verse/air@v1.61.1 \
    && chmod -R a+rwX /go/pkg/mod \
    && mkdir -p /src/.air-tmp \
    && chmod a+rwx /src/.air-tmp
ENV GOCACHE=/tmp/go-build
COPY .air.toml ./.air.toml
CMD ["air", "-c", ".air.toml"]

# Applies the schema of this version with Atlas Community Edition (Apache-2.0).
FROM arigaio/atlas:1.2.2-community AS migrate
COPY db/sqlite/schema.hcl /migrate/sqlite/schema.hcl
COPY db/postgres/schema.hcl /migrate/postgres/schema.hcl
COPY deploy/atlas.hcl /migrate/atlas.hcl
# Owned by the app user so that a new named volume first mounted here stays
# writable for the app.
COPY --from=backend --chown=1000:1000 /out/data/ /app/data/
WORKDIR /migrate
USER 1000:1000
# Do not contact Ariga for update checks on every run.
ENV ATLAS_NO_UPDATE_NOTIFIER=1 ATLAS_NO_UPGRADE_SUGGESTIONS=1

FROM scratch
COPY --from=backend /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=backend /out/astovia /astovia
COPY --from=backend --chown=1000:1000 /out/data/ /app/data/
WORKDIR /app
USER 1000:1000
# Listen on all interfaces inside the container; direct runs keep 127.0.0.1.
ENV ASTOVIA_ADDR=0.0.0.0:8080
EXPOSE 8080
ENTRYPOINT ["/astovia"]
CMD ["serve"]
