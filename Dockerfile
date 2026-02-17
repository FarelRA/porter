FROM golang:1.24-alpine AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

COPY go.mod ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -trimpath -ldflags "-s -w" -o /out/porter .

FROM alpine:3.20

RUN adduser -D -h /app porter
WORKDIR /app

COPY --from=build /out/porter /app/porter
COPY web /app/web

ENV PORTER_HTTP_ADDR=:9876
ENV PORTER_DATA_DIR=/config

EXPOSE 9876
VOLUME ["/config"]

ENTRYPOINT ["/app/porter"]
