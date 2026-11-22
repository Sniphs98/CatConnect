# CatConnect server: runs next to your game servers.
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /catconnect-server ./cmd/catconnect-server

# Runs as root inside the container so a bind-mounted ./catconnect-data
# folder is writable without chown; the binary needs no other privileges.
FROM gcr.io/distroless/static-debian12
COPY --from=build /catconnect-server /catconnect-server
VOLUME /data
ENV CATCONNECT_DATA=/data
EXPOSE 8080
ENTRYPOINT ["/catconnect-server"]
