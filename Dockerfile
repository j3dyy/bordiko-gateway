# Standalone image for the Bordiko gateway.
#
# The build context is THIS directory, with no dependency on the monorepo's
# go.work — so the service can be extracted into its own repo and built as-is.
FROM golang:1.26-bookworm AS build
ENV CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -o /out/app .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
EXPOSE 8080
ENTRYPOINT ["/app"]
