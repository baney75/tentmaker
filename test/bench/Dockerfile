# The bench Docker workload (scripts/bench/docker-build.sh): tentmaker built
# in a pinned Go image and copied into a pinned Alpine image. Base images
# come from the ECR Public mirror of Docker's official images, which has no
# anonymous pull limit, and are pinned by digest.
FROM public.ecr.aws/docker/library/golang:1.27-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/tentmaker ./cmd/tentmaker

FROM public.ecr.aws/docker/library/alpine:3.22@sha256:49340b346798875b5592e5d420241f2f9cfba5f47e62ebb1446a1b34734e898f
COPY --from=build /out/tentmaker /usr/local/bin/tentmaker
ENTRYPOINT ["tentmaker"]
