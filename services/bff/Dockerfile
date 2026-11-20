# マルチステージビルド: Go でビルドし、シェルも持たない distroless イメージで動かす
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app .

FROM build AS test-build
RUN CGO_ENABLED=0 go test -c -o /out/session-tests .

FROM gcr.io/distroless/static-debian12:nonroot AS tests
COPY --from=test-build /out/session-tests /tests
USER nonroot:nonroot
ENTRYPOINT ["/tests", "-test.v", "-test.run", "TestRedis"]

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/app"]
