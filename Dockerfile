FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go test ./... \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/vep ./cmd/vep \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/vhsim ./cmd/vhsim \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot AS sim
LABEL org.opencontainers.image.source="https://github.com/sethdotfm/sode-VideohubEthernetProtocol"
COPY --from=build /out/vhsim /vhsim
EXPOSE 9990
ENTRYPOINT ["/vhsim"]

FROM gcr.io/distroless/static-debian12:nonroot AS vep
LABEL org.opencontainers.image.source="https://github.com/sethdotfm/sode-VideohubEthernetProtocol"
COPY --from=build /out/vep /vep
COPY --from=build --chown=65532:65532 /out/data /data
ENV VEP_CONFIG=/data/config.yaml
VOLUME /data
EXPOSE 8081 8910
HEALTHCHECK --interval=30s --timeout=3s CMD ["/vep", "-healthcheck"]
ENTRYPOINT ["/vep"]
