FROM golang:1.27.3-alpine AS build
WORKDIR /src
ARG VERSION=dev
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN for command in server migrate clients; do \
      CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/$command ./cmd/$command || exit 1; \
    done

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /app/
ENV HTTP_ADDR=0.0.0.0:4010
EXPOSE 4010
USER nonroot:nonroot
ENTRYPOINT ["/app/server"]
