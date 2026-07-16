FROM golang:1.26 AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/feedworker ./cmd/feedworker

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=build /out/api /api
COPY --from=build /out/feedworker /feedworker
COPY --from=build /app/data/simulator /data/simulator
ENV SIMULATOR_DATA_DIR=/data/simulator
EXPOSE 8080
CMD ["/api"]
