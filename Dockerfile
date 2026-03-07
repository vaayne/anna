FROM --platform=$BUILDPLATFORM golang:1.24 AS builder

WORKDIR /go/src/app
COPY go.mod go.sum ./
RUN go mod download

ARG TARGETOS TARGETARCH
ARG VERSION=dev
ENV CGO_ENABLED=0

COPY . .
RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags="-s -w" -o bin/anna .

FROM gcr.io/distroless/static-debian13:nonroot AS app
WORKDIR /workspace

COPY --from=builder /go/src/app/bin/anna .

USER nonroot:nonroot

CMD ["./anna", "gateway"]
