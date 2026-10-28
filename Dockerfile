FROM golang:1.24-alpine AS build
WORKDIR /src
COPY . .
# go.sum is generated on first build if it is missing.
RUN go mod tidy \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/paygate ./cmd/paygate \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/edge ./cmd/edge

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/paygate /paygate
COPY --from=build /out/edge /edge
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/paygate"]
