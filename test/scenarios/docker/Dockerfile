# The docker-cache scenario's build (test/scenarios/docker-cache.sh): two
# stages, both base images pinned by digest, so a cold build pulls the same
# bytes every time and a warm one should pull nothing and reuse every layer.
FROM golang:1.24-alpine@sha256:8bee1901f1e530bfb4a7850aa7a479d17ae3a18beb6e09064ed54cfd245b7191 AS build
WORKDIR /src
COPY hello.go .
RUN CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /hello hello.go

FROM alpine:3.21@sha256:bc21487101ae47172509130ee1092cc273a831968d64b617776d1f69c81e42ab
RUN apk add --no-cache ca-certificates
COPY --from=build /hello /usr/local/bin/hello
CMD ["hello"]
