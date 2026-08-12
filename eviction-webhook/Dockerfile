FROM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o /eviction-webhook ./cmd/eviction-webhook

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /eviction-webhook /eviction-webhook
ENTRYPOINT ["/eviction-webhook"]
