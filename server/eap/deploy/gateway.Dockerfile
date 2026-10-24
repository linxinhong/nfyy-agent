# Portal web build from the xingding-ui-cli generated app (ADR-0003).
# The CLI itself is not a runtime dependency; this stage only builds its output.
FROM node:22-alpine AS web
# pnpm pinned to the lockfile-producing major (pnpm 11's supply-chain policy
# rejects lockfiles resolved by pnpm 9; see ADR-0007).
RUN npm install -g pnpm@9.12.2
WORKDIR /web
COPY apps/web/package.json apps/web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY apps/web/ .
RUN pnpm exec vite build

FROM golang:1.27-alpine AS build
# GOPROXY is supplied by the caller (Makefile passes the host's setting);
# the default keeps plain `docker build` working on unrestricted networks.
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/eap-gateway ./apps/gateway

FROM alpine:3.20
RUN adduser -D -u 10001 eap
USER eap
COPY --from=build /out/eap-gateway /usr/local/bin/eap-gateway
COPY --from=build --chown=eap /src/migrations /migrations
COPY --from=web --chown=eap /web/dist /web/dist
ENV EAP_MIGRATIONS_DIR=/migrations
ENV EAP_WEB_DIST=/web/dist
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/eap-gateway"]
