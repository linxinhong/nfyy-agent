FROM golang:1.26-alpine@sha256:8ac98ca534ac3f51e1f420a1dd2c15e74c75cfa0f23f3ad27eb5d7236c349a0c AS build
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
ENV CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api ./api
COPY cmd ./cmd
COPY internal ./internal
COPY migrations ./migrations
RUN go build -trimpath -ldflags="-s -w" -o /out/yuanpu-server ./cmd/yuanpu-server

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/yuanpu-server /yuanpu-server
USER 65532:65532
ENV YUANPU_SERVER_LISTEN=0.0.0.0:8788
EXPOSE 8788
ENTRYPOINT ["/yuanpu-server"]
CMD ["serve"]
