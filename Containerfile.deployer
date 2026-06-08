FROM golang:1.25 AS builder
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/deployer ./cmd/deployer
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/openclaw-deployer ./cmd/deployer

FROM registry.access.redhat.com/ubi9-micro:latest
COPY --from=builder /out/openclaw-deployer /usr/local/bin/openclaw-deployer
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/openclaw-deployer"]
