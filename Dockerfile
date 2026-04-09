FROM golang:1.23 AS builder

WORKDIR /src

COPY go.mod ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o /out/review-dashboard .

FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=builder /out/review-dashboard /app/review-dashboard

EXPOSE 8080

ENV PORT=8080

ENTRYPOINT ["/app/review-dashboard"]