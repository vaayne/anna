FROM golang:1.24 AS builder

WORKDIR /go/src/app
COPY go.mod go.sum ./
RUN go mod download

ARG VERSION=dev
ENV CGO_ENABLED=0 GOOS=linux

COPY . .
RUN go build -ldflags="-s -w" -o bin/anna .

FROM gcr.io/distroless/static-debian13:nonroot AS app
WORKDIR /service

COPY --from=builder /go/src/app/bin/anna .

USER nonroot:nonroot

CMD ["./anna", "gateway"]
