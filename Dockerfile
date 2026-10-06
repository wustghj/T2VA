FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags='-s -w' -o /out/cloud ./cmd/cloud

FROM alpine:3.21
RUN adduser -D -u 10001 app && mkdir -p /data && chown app:app /data
USER app
WORKDIR /app
COPY --from=build /out/cloud /usr/local/bin/cloud
COPY web ./web
ENV ADDR=:8080 DATABASE_PATH=/data/t2va.db STORAGE_PATH=/data/files
EXPOSE 8080
ENTRYPOINT ["cloud"]
